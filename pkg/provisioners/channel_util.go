package provisioners

import (
	"context"
	"fmt"

	istiov1alpha3 "github.com/knative/pkg/apis/istio/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/knative/eventing/pkg/apis/eventing/v1alpha1"
	"github.com/knative/eventing/pkg/controller"
	"github.com/knative/eventing/pkg/system"
	"k8s.io/apimachinery/pkg/api/equality"
)

const (
	PortName   = "http"
	PortNumber = 80
)

// AddFinalizerResult is used indicate whether a finalizer was added or already present.
type AddFinalizerResult bool

const (
	FinalizerAlreadyPresent AddFinalizerResult = false
	FinalizerAdded          AddFinalizerResult = true
)

// AddFinalizer adds finalizerName to the Channel.
func AddFinalizer(c *eventingv1alpha1.Channel, finalizerName string) AddFinalizerResult {
	finalizers := sets.NewString(c.Finalizers...)
	if finalizers.Has(finalizerName) {
		return FinalizerAlreadyPresent
	}
	finalizers.Insert(finalizerName)
	c.Finalizers = finalizers.List()
	return FinalizerAdded
}

func RemoveFinalizer(c *eventingv1alpha1.Channel, finalizerName string) {
	finalizers := sets.NewString(c.Finalizers...)
	finalizers.Delete(finalizerName)
	c.Finalizers = finalizers.List()
}

func CreateK8sService(ctx context.Context, client runtimeClient.Client, c *eventingv1alpha1.Channel) (*corev1.Service, error) {
	svcKey := types.NamespacedName{
		Namespace: c.Namespace,
		Name:      ChannelServiceName(c.Name),
	}
	return createK8sService(ctx, client, svcKey, newK8sService(c))
}

func createK8sService(ctx context.Context, client runtimeClient.Client, svcKey types.NamespacedName, svc *corev1.Service) (*corev1.Service, error) {
	current := &corev1.Service{}
	err := client.Get(ctx, svcKey, current)

	if k8serrors.IsNotFound(err) {
		err = client.Create(ctx, svc)
		if err != nil {
			return nil, err
		}
		return svc, nil
	} else if err != nil {
		return nil, err
	}

	// spec.clusterIP is immutable and is set on existing services. If we don't set this
	// to the same value, we will encounter an error while updating.
	svc.Spec.ClusterIP = current.Spec.ClusterIP
	if !equality.Semantic.DeepDerivative(svc.Spec, current.Spec) {
		current.Spec = svc.Spec
		err = client.Update(ctx, current)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func getVirtualService(ctx context.Context, client runtimeClient.Client, c *eventingv1alpha1.Channel) (*istiov1alpha3.VirtualService, error) {
	vsk := runtimeClient.ObjectKey{
		Namespace: c.Namespace,
		Name:      ChannelVirtualServiceName(c.ObjectMeta.Name),
	}
	vs := &istiov1alpha3.VirtualService{}
	err := client.Get(ctx, vsk, vs)
	return vs, err
}

func CreateVirtualService(ctx context.Context, client runtimeClient.Client, channel *eventingv1alpha1.Channel) (*istiov1alpha3.VirtualService, error) {
	virtualService, err := getVirtualService(ctx, client, channel)

	// If the resource doesn't exist, we'll create it
	if k8serrors.IsNotFound(err) {
		virtualService = newVirtualService(channel)
		err = client.Create(ctx, virtualService)
		if err != nil {
			return nil, err
		}
		return virtualService, nil
	} else if err != nil {
		return nil, err
	}

	// Update VirtualService if it has changed. This is possible since in version 0.2.0, the destinationHost in
	// spec.HTTP.Route for the dispatcher was changed from *-clusterbus to *-dispatcher. Even otherwise, this
	// reconciliation is useful for the future mutations to the object.
	expected := newVirtualService(channel)
	if !equality.Semantic.DeepDerivative(expected.Spec, virtualService.Spec) {
		virtualService.Spec = expected.Spec
		err := client.Update(ctx, virtualService)
		if err != nil {
			return nil, err
		}
	}
	return virtualService, nil
}

func UpdateChannel(ctx context.Context, client runtimeClient.Client, u *eventingv1alpha1.Channel) error {
	channel := &eventingv1alpha1.Channel{}
	err := client.Get(ctx, runtimeClient.ObjectKey{Namespace: u.Namespace, Name: u.Name}, channel)
	if err != nil {
		return err
	}

	updated := false
	if !equality.Semantic.DeepEqual(channel.Finalizers, u.Finalizers) {
		channel.SetFinalizers(u.ObjectMeta.Finalizers)
		updated = true
	}

	if !equality.Semantic.DeepEqual(channel.Status, u.Status) {
		channel.Status = u.Status
		updated = true
	}

	if updated {
		return client.Update(ctx, channel)
	}
	return nil
}

// newK8sService creates a new Service for a Channel resource. It also sets the appropriate
// OwnerReferences on the resource so handleObject can discover the Channel resource that 'owns' it.
// As well as being garbage collected when the Channel is deleted.
func newK8sService(c *eventingv1alpha1.Channel) *corev1.Service {
	labels := map[string]string{
		"channel":     c.Name,
		"provisioner": c.Spec.Provisioner.Name,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChannelServiceName(c.ObjectMeta.Name),
			Namespace: c.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(c, schema.GroupVersionKind{
					Group:   eventingv1alpha1.SchemeGroupVersion.Group,
					Version: eventingv1alpha1.SchemeGroupVersion.Version,
					Kind:    "Channel",
				}),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: PortName,
					Port: PortNumber,
				},
			},
		},
	}
}

// newVirtualService creates a new VirtualService for a Channel resource. It also sets the
// appropriate OwnerReferences on the resource so handleObject can discover the Channel resource
// that 'owns' it. As well as being garbage collected when the Channel is deleted.
func newVirtualService(channel *eventingv1alpha1.Channel) *istiov1alpha3.VirtualService {
	labels := map[string]string{
		"channel":     channel.Name,
		"provisioner": channel.Spec.Provisioner.Name,
	}
	destinationHost := controller.ServiceHostName(ChannelDispatcherServiceName(channel.Spec.Provisioner.Name), system.Namespace)
	return &istiov1alpha3.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChannelVirtualServiceName(channel.Name),
			Namespace: channel.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(channel, schema.GroupVersionKind{
					Group:   eventingv1alpha1.SchemeGroupVersion.Group,
					Version: eventingv1alpha1.SchemeGroupVersion.Version,
					Kind:    "Channel",
				}),
			},
		},
		Spec: istiov1alpha3.VirtualServiceSpec{
			Hosts: []string{
				controller.ServiceHostName(ChannelServiceName(channel.Name), channel.Namespace),
				ChannelHostName(channel.Name, channel.Namespace),
			},
			Http: []istiov1alpha3.HTTPRoute{{
				Rewrite: &istiov1alpha3.HTTPRewrite{
					Authority: ChannelHostName(channel.Name, channel.Namespace),
				},
				Route: []istiov1alpha3.DestinationWeight{{
					Destination: istiov1alpha3.Destination{
						Host: destinationHost,
						Port: istiov1alpha3.PortSelector{
							Number: PortNumber,
						},
					}},
				}},
			},
		},
	}
}

func ChannelVirtualServiceName(channelName string) string {
	return fmt.Sprintf("%s-channel", channelName)
}

func ChannelServiceName(channelName string) string {
	return fmt.Sprintf("%s-channel", channelName)
}

func ChannelHostName(channelName, namespace string) string {
	return fmt.Sprintf("%s.%s.channels.cluster.local", channelName, namespace)
}
