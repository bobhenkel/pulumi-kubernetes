package await

import (
	"fmt"
	"reflect"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-kubernetes/pkg/client"
	"github.com/pulumi/pulumi-kubernetes/pkg/openapi"
	"github.com/pulumi/pulumi/pkg/diag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// ------------------------------------------------------------------------------------------------

// Await logic for core/v1/Service.
//
// The goal of this code is to provide a fine-grained account of the status of a Kubernetes Service
// as it is being initialized. The idea is that if something goes wrong early, we want to alert the
// user so they can cancel the operation instead of waiting for timeout (~10 minutes).
//
// A Service can be one of several types, and the initialization behavior differs for each:
//
//   - If the type is `LoadBalancer`, the Service will be allocated a public IP address, and an
//     Endpoint object will be created, which specifies to which Pods traffic on different ports is
//     should be directed.
//   - If the type is `ClusterIP`, the Service is directly addressable only from inside the
//     cluster, so no public IP address will be allocated. An Endpoint object will still be created
//     to specify to which Pods traffic on different ports should be directed.
//
// The design of this awaiter is fundamentally an event loop on five channels:
//
//   1. The Service channel, to which the Kubernetes API server will proactively push every change
//      (additions, modifications, deletions) to any Service it knows about.
//   2. The Endpoint channel, which is the same idea as the Service channel, except it gets updates
//      to Endpoint objects.
//   3. A timeout channel, which fires after some minutes.
//   4. A cancellation channel, with which the user can signal cancellation (e.g., using SIGINT).
//   5. A "settled" channel, which is meant to fire a few seconds after any update to an Endpoint
//      object, so that we're sure they have time to "settle".
//
// The `serviceInitAwaiter` will synchronously process events from the union of all these channels.
// Any time the success conditions described above a reached, we will terminate the awaiter.
//
// The intermediate status we report tends to be related to whether endpoints are targeting > 0
// Pods. Because an external IP can take a long time to execute, we simply have to wait.
//
//
// x-refs:
//   * https://kubernetes.io/docs/tutorials/services/

// ------------------------------------------------------------------------------------------------

type serviceInitAwaiter struct {
	config           createAwaitConfig
	serviceReady     bool
	endpointsReady   bool
	endpointsSettled bool
}

func makeServiceInitAwaiter(c createAwaitConfig) *serviceInitAwaiter {
	return &serviceInitAwaiter{
		config:           c,
		serviceReady:     false,
		endpointsReady:   false,
		endpointsSettled: false,
	}
}

func awaitServiceInit(c createAwaitConfig) error {
	return makeServiceInitAwaiter(c).Await()
}

func (sia *serviceInitAwaiter) Await() error {
	//
	// We succeed only when all of the following are true:
	//
	//   1. Service object exists.
	//   2. Endpoint objects created. Each time we get an update, wait ~5-10 seconds
	//      after update to wait for any stragglers.
	//   3. The endpoints objects target some number of living objects.
	//   4. External IP address is allocated (if we're type `LoadBalancer`).
	//

	// Create service watcher.
	serviceWatcher, err := sia.config.clientForResource.Watch(metav1.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "Could set up watch for Service object '%s'",
			sia.config.currentInputs.GetName())
	}
	defer serviceWatcher.Stop()

	// Create endpoint watcher.
	endpointClient, err := client.FromGVK(sia.config.pool, sia.config.disco, schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Endpoints",
	}, sia.config.currentInputs.GetNamespace())
	if err != nil {
		return errors.Wrapf(err,
			"Could not make client to watch Endpoint object associated with Service '%s'",
			sia.config.currentInputs.GetName())
	}

	endpointWatcher, err := endpointClient.Watch(metav1.ListOptions{})
	if err != nil {
		return errors.Wrapf(err,
			"Could not create watcher for Endpoint objects associated with Service '%s'",
			sia.config.currentInputs.GetName())
	}
	defer endpointWatcher.Stop()

	return sia.await(serviceWatcher, endpointWatcher, time.After(10*time.Minute), make(chan struct{}))
}

func (sia *serviceInitAwaiter) Read() error {
	// Get live versions of Service and Endpoints.
	service, err := sia.config.clientForResource.Get(sia.config.currentInputs.GetName(),
		metav1.GetOptions{})
	if err != nil {
		// IMPORTANT: Do not wrap this error! If this is a 404, the provider need to know so that it
		// can mark the deployment as having been deleted.
		return err
	}

	//
	// In contrast to the case of `deployment`, an error getting the ReplicaSet or Pod lists does
	// not indicate that this resource was deleted, and we therefore should report the wrapped error
	// in a way that is useful to the user.
	//

	// Create endpoint watcher.
	endpointClient, err := client.FromGVK(sia.config.pool, sia.config.disco, schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Endpoints",
	}, sia.config.currentInputs.GetNamespace())
	if err != nil {
		return errors.Wrapf(err,
			"Could not make client to list Endpoint object associated with Service '%s'",
			sia.config.currentInputs.GetName())
	}

	endpointList, err := endpointClient.List(metav1.ListOptions{})
	if err != nil {
		glog.V(3).Infof("Error retrieving ReplicaSet list for Service '%s': %v",
			service.GetName(), err)
		endpointList = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{}}
	}

	return sia.read(service, endpointList.(*unstructured.UnstructuredList))
}

func (sia *serviceInitAwaiter) read(
	service *unstructured.Unstructured, endpoints *unstructured.UnstructuredList,
) error {
	sia.processServiceEvent(watchAddedEvent(service))

	var err error
	settled := make(chan struct{})
	err = endpoints.EachListItem(func(endpoint runtime.Object) error {
		sia.processEndpointEvent(watchAddedEvent(endpoint.(*unstructured.Unstructured)), settled)
		return nil
	})
	if err != nil {
		glog.V(3).Infof("Error iterating over ReplicaSet list for Deployment '%s': %v",
			service.GetName(), err)
	}

	sia.endpointsSettled = true

	if sia.succeeded() {
		return nil
	}

	return &initializationError{
		subErrors: sia.errorMessages(),
		object:    service,
	}
}

// await is a helper companion to `Await` designed to make it easy to test this module.
func (sia *serviceInitAwaiter) await(
	serviceWatcher, endpointWatcher watch.Interface, timeout <-chan time.Time,
	settled chan struct{},
) error {
	inputServiceName := sia.config.currentInputs.GetName()
	for {
		// Check whether we've succeeded.
		if sia.serviceReady && sia.endpointsSettled && sia.endpointsReady {
			return nil
		}

		// Else, wait for updates.
		select {
		case <-sia.config.ctx.Done():
			// On cancel, check one last time if the service is ready.
			if sia.serviceReady && sia.endpointsReady {
				return nil
			}
			return &cancellationError{
				objectName: inputServiceName,
				subErrors:  sia.errorMessages(),
			}
		case <-timeout:
			// On timeout, check one last time if the service is ready.
			if sia.serviceReady && sia.endpointsReady {
				return nil
			}
			return &timeoutError{
				objectName: inputServiceName,
				subErrors:  sia.errorMessages(),
			}
		case <-settled:
			var message string
			sev := diag.Warning
			if sia.endpointsReady {
				message = fmt.Sprintf("✅ Service '%s' successfully created endpoint objects\n",
					inputServiceName)
				sev = diag.Info
			} else {
				message = fmt.Sprintf("Service '%s' does not target any Pods\n", inputServiceName)
			}

			if sia.config.host != nil {
				_ = sia.config.host.Log(sia.config.ctx, sev, sia.config.urn, message)
			}
			sia.endpointsSettled = true
		case event := <-serviceWatcher.ResultChan():
			sia.processServiceEvent(event)
		case event := <-endpointWatcher.ResultChan():
			sia.processEndpointEvent(event, settled)
		}
	}
}

func (sia *serviceInitAwaiter) processServiceEvent(event watch.Event) {
	inputServiceName := sia.config.currentInputs.GetName()

	service, isUnstructured := event.Object.(*unstructured.Unstructured)
	if !isUnstructured {
		glog.V(3).Infof("Service watch received unknown object type '%s'",
			reflect.TypeOf(service))
		return
	}

	// Do nothing if this is not the service we're waiting for.
	if service.GetName() != inputServiceName {
		return
	}

	// Start with a blank slate.
	sia.serviceReady = false

	// Mark the service as not ready if it's deleted.
	if event.Type == watch.Deleted {
		return
	}

	specType, _ := openapi.Pluck(sia.config.currentInputs.Object, "spec", "type")
	if fmt.Sprintf("%v", specType) == string(v1.ServiceTypeLoadBalancer) {
		// If it's type `LoadBalancer`, check whether an IP was allocated.
		lbIngress, _ := openapi.Pluck(service.Object, "status", "loadBalancer", "ingress")
		status, _ := openapi.Pluck(service.Object, "status")

		glog.V(3).Infof("Received status for service '%s': %#v", inputServiceName, status)
		ing, isSlice := lbIngress.([]interface{})

		// Update status of service object so that we can check success.
		sia.serviceReady = isSlice && len(ing) > 0

		if sia.serviceReady {
			if sia.config.host != nil {
				_ = sia.config.host.Log(sia.config.ctx, diag.Info, sia.config.urn,
					"✅ Service has been allocated an IP")
			}
		}
		glog.V(3).Infof("Waiting for service '%q' to assign IP/hostname for a load balancer",
			inputServiceName)
	} else {
		// If it's not type `LoadBalancer`, report success.
		sia.serviceReady = true
	}
}

func (sia *serviceInitAwaiter) processEndpointEvent(event watch.Event, settledCh chan<- struct{}) {
	inputServiceName := sia.config.currentInputs.GetName()

	// Get endpoint object.
	endpoint, isUnstructured := event.Object.(*unstructured.Unstructured)
	if !isUnstructured {
		glog.V(3).Infof("Endpoint watch received unknown object type '%s'",
			reflect.TypeOf(endpoint))
		return
	}

	// Ignore if it's not one of the endpoint objects created by the service.
	//
	// NOTE: Because the client pool is per-namespace, the endpointName can be used as an
	// ID, as it's guaranteed by the API server to be unique.
	if endpoint.GetName() != inputServiceName {
		return
	}

	// Start over, prove that service is ready.
	sia.endpointsReady = false

	// Update status of endpoint objects so we can check success.
	if event.Type == watch.Added || event.Type == watch.Modified {
		subsets, hasTargets := openapi.Pluck(endpoint.Object, "subsets")
		targets, targetsIsSlice := subsets.([]interface{})
		endpointTargetsPod := hasTargets && targetsIsSlice && len(targets) > 0

		sia.endpointsReady = endpointTargetsPod
	} else if event.Type == watch.Deleted {
		sia.endpointsReady = false
	}

	// Every time we get an update to one of our endpoints objects, give it a few seconds
	// for them to settle.
	sia.endpointsSettled = false
	go func() {
		time.Sleep(10 * time.Second)
		settledCh <- struct{}{}
	}()
}

func (sia *serviceInitAwaiter) errorMessages() []string {
	messages := []string{}
	if !sia.endpointsReady {
		messages = append(messages, "Service does not target any Pods")
	}

	specType, _ := openapi.Pluck(sia.config.currentInputs.Object, "spec", "type")
	if fmt.Sprintf("%v", specType) == string(v1.ServiceTypeLoadBalancer) && !sia.serviceReady {
		messages = append(messages, "Service was not allocated an IP address")
	}

	return messages
}

func (sia *serviceInitAwaiter) collectWarningEvents() error {
	clientForEvents, err := sia.config.eventClient()
	if err != nil {
		glog.V(3).Infof("Could not retrieve warning events for service '%s': %v",
			sia.config.currentInputs.GetName(), err)
	}
	lastWarnings, wErr := getLastWarningsForObject(clientForEvents,
		sia.config.currentInputs.GetNamespace(),
		sia.config.currentInputs.GetName(), "Service", 3)
	if wErr != nil {
		glog.V(3).Infof("Could not retrieve warning events for service '%s': %v",
			sia.config.currentInputs.GetName(), wErr)
	}
	return fmt.Errorf("%s%s", err, stringifyEvents(lastWarnings))
}

func (sia *serviceInitAwaiter) succeeded() bool {
	return sia.serviceReady && sia.endpointsSettled && sia.endpointsReady
}
