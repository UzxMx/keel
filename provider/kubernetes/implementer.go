package kubernetes

import (
	"fmt"
	"sync"

	"github.com/keel-hq/keel/internal/k8s"

	apps_v1 "k8s.io/api/apps/v1"
	batch_v1 "k8s.io/api/batch/v1"
	v1beta1 "k8s.io/api/batch/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	core_v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	// core_v1 "k8s.io/api/core/v1"
	// "k8s.io/api/core/v1"

	"k8s.io/api/core/v1"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	log "github.com/sirupsen/logrus"
)

// Implementer - thing wrapper around currently used k8s APIs
type Implementer interface {
	Namespaces() (*v1.NamespaceList, error)

	// Deployment(namespace, name string) (*v1beta1.Deployment, error)
	Deployments(namespace string) (*apps_v1.DeploymentList, error)
	// Update(deployment *v1beta1.Deployment) error
	Update(obj *k8s.GenericResource) error

	Secret(namespace, name string) (*v1.Secret, error)

	Pods(namespace, labelSelector string) (*v1.PodList, error)
	DeletePod(namespace, name string, opts *meta_v1.DeleteOptions) error

	ConfigMaps(namespace string) core_v1.ConfigMapInterface
}

// KubernetesImplementer - default kubernetes client implementer, uses
// https://github.com/kubernetes/client-go v3.0.0-beta.0
type KubernetesImplementer struct {
	mu          sync.Mutex
	cfg         *rest.Config
	client      *kubernetes.Clientset
	pendingJobs []*batch_v1.Job
}

// Opts - implementer options, usually for k8s deployments
// it's best to use InCluster option
type Opts struct {
	// if set - kube config options will be ignored
	InCluster  bool
	ConfigPath string
	Master     string
}

// NewKubernetesImplementer - create new k8s implementer
func NewKubernetesImplementer(opts *Opts) (*KubernetesImplementer, error) {
	cfg := &rest.Config{}

	if opts.InCluster {
		var err error
		cfg, err = rest.InClusterConfig()
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("provider.kubernetes: failed to get kubernetes config")
			return nil, err
		}
		log.Info("provider.kubernetes: using in-cluster configuration")
	} else if opts.ConfigPath != "" {
		var err error
		cfg, err = clientcmd.BuildConfigFromFlags("", opts.ConfigPath)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("provider.kubernetes: failed to get cmd kubernetes config")
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("kubernetes config is missing")
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("provider.kubernetes: failed to create kubernetes client")
		return nil, err
	}

	return &KubernetesImplementer{client: client, cfg: cfg}, nil
}

func (i *KubernetesImplementer) Client() *kubernetes.Clientset {
	return i.client
}

// Namespaces - get all namespaces
func (i *KubernetesImplementer) Namespaces() (*v1.NamespaceList, error) {
	namespaces := i.client.Core().Namespaces()
	return namespaces.List(meta_v1.ListOptions{})
}

// Deployment - get specific deployment for namespace/name
func (i *KubernetesImplementer) Deployment(namespace, name string) (*apps_v1.Deployment, error) {
	dep := i.client.Apps().Deployments(namespace)
	return dep.Get(name, meta_v1.GetOptions{})
}

// Deployments - get all deployments for namespace
func (i *KubernetesImplementer) Deployments(namespace string) (*apps_v1.DeploymentList, error) {
	dep := i.client.Apps().Deployments(namespace)
	l, err := dep.List(meta_v1.ListOptions{})
	return l, err
}

// Update converts generic resource into specific kubernetes type and updates it
func (i *KubernetesImplementer) Update(obj *k8s.GenericResource) error {
	// retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
	// 	// Retrieve the latest version of Deployment before attempting update
	// 	// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
	// 	_, updateErr := i.client.Extensions().Deployments(deployment.Namespace).Update(deployment)
	// 	return updateErr
	// })
	// return retryErr

	i.mu.Lock()
	defer i.mu.Unlock()

	switch resource := obj.GetResource().(type) {
	case *apps_v1.Deployment:
		_, err := i.client.Apps().Deployments(resource.Namespace).Update(resource)
		if err != nil {
			return err
		}
	case *apps_v1.StatefulSet:
		_, err := i.client.Apps().StatefulSets(resource.Namespace).Update(resource)
		if err != nil {
			return err
		}
	case *apps_v1.DaemonSet:
		_, err := i.client.Apps().DaemonSets(resource.Namespace).Update(resource)
		if err != nil {
			return err
		}
	case *batch_v1.Job:
		if resource.Status.CompletionTime != nil {
			policy := meta_v1.DeletePropagationBackground
			err := i.client.BatchV1().Jobs(resource.Namespace).Delete(resource.Name, &meta_v1.DeleteOptions{
				PropagationPolicy: &policy,
			})
			if err != nil {
				log.Info("Delete error")
				return err
			}
			delete(resource.Spec.Template.ObjectMeta.Labels, "controller-uid")
			resource.Spec.Selector = nil
			resource.ObjectMeta.ResourceVersion = ""
			i.pendingJobs = append(i.pendingJobs, resource)
		}
	case *v1beta1.CronJob:
		_, err := i.client.BatchV1beta1().CronJobs(resource.Namespace).Update(resource)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported object type")
	}
	return nil
}

// Secret - get secret
func (i *KubernetesImplementer) Secret(namespace, name string) (*v1.Secret, error) {

	return i.client.Core().Secrets(namespace).Get(name, meta_v1.GetOptions{})
}

// Pods - get pods
func (i *KubernetesImplementer) Pods(namespace, labelSelector string) (*v1.PodList, error) {
	return i.client.Core().Pods(namespace).List(meta_v1.ListOptions{LabelSelector: labelSelector})
}

// DeletePod - delete pod by name
func (i *KubernetesImplementer) DeletePod(namespace, name string, opts *meta_v1.DeleteOptions) error {
	return i.client.Core().Pods(namespace).Delete(name, opts)
}

// ConfigMaps - returns an interface to config maps for a specified namespace
func (i *KubernetesImplementer) ConfigMaps(namespace string) core_v1.ConfigMapInterface {
	return i.client.Core().ConfigMaps(namespace)
}

func (i *KubernetesImplementer) OnAdd(gr *k8s.GenericResource) {
}

func (i *KubernetesImplementer) OnUpdate(gr *k8s.GenericResource) {
}

func (i *KubernetesImplementer) OnDelete(gr *k8s.GenericResource) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if resource, ok := gr.GetResource().(*batch_v1.Job); ok {
		for idx, job := range i.pendingJobs {
			if job.Namespace == resource.Namespace && job.Name == resource.Name {
				_, err := i.client.BatchV1().Jobs(job.Namespace).Create(job)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
					}).Error("Failed to create job")
				} else {
					log.WithFields(log.Fields{
						"namespace": job.Namespace,
						"name":      job.Name,
					}).Info("Job created")
				}
				i.pendingJobs = append(i.pendingJobs[:idx], i.pendingJobs[idx+1:]...)
				return
			}
		}
	}
}
