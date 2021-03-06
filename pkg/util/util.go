package util

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/time/rate"
	"os"
	"reflect"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	types "github.com/kubernetes-sigs/container-object-storage-interface-api/apis/objectstorage.k8s.io/v1alpha1"
	bucketclientset "github.com/kubernetes-sigs/container-object-storage-interface-api/clientset"
	"github.com/kubernetes-sigs/container-object-storage-interface-api/controller"

	"sigs.k8s.io/controller-tools/pkg/crd"
	crdmarkers "sigs.k8s.io/controller-tools/pkg/crd/markers"
	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"

	"github.com/golang/glog"
)

var (
	// Error codes that the central controller will return
	ErrBucketAlreadyExists = errors.New("A bucket already existing that matches the bucket request")
	ErrInvalidBucketClass  = errors.New("Cannot find Bucket Class with the name specified in the bucket request")
	ErrBCUnavailable       = errors.New("BucketClass is not available")
	ErrNotImplemented      = errors.New("Operation Not Implemented")
)

func CopySS(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	copy := make(map[string]string, len(m))
	for k, v := range m {
		copy[k] = v
	}
	return copy
}

func CopyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	c := make([]string, len(s))
	copy(c, s)
	return c
}

func GetUUID() string {
	return string(uuid.NewUUID())
}

// SetupTest is utility function to create clients and controller
// This is used by bucket request and bucket access request unit tests
func SetupTest(ctx context.Context) (bucketclientset.Interface, kubeclientset.Interface, *controller.ObjectStorageController) {

	// Initialize the clients
	config, err := func() (*rest.Config, error) {
		kubeConfig := os.Getenv("KUBECONFIG")
		if kubeConfig != "" {
			return clientcmd.BuildConfigFromFlags("", kubeConfig)
		}
		return rest.InClusterConfig()
	}()
	if err != nil {
		glog.Fatalf("Failed to create clients: %v", err)
	}

	kubeClient, err := kubeclientset.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	client, err := bucketclientset.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create ObjectStorage client: %v", err)
	}

	crdClientset, err := apiextensions.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create CRD client: %v", err)
	}

	err = RegisterCRDs(ctx, crdClientset.CustomResourceDefinitions())
	if err != nil {
		glog.Fatalf("Failed to register CRDs: %v", err)
	}

	rateLimit := workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(100*time.Millisecond, 600*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
	ctrl, err := controller.NewObjectStorageControllerWithClientset("controller-manager", "leader-lock", 40, rateLimit, kubeClient, client)
	if err != nil {
		glog.Fatalf("Failed to create ObjectStorage Controller: %v", err)
	}

	go ctrl.Run(ctx)
	return client, kubeClient, ctrl
}

// GetBuckets will wait and fetch expected number of buckets created by the test
// This is used by bucket request unit tests
func GetBuckets(ctx context.Context, client bucketclientset.Interface, numExpected int) *types.BucketList {
	bucketList, err := client.ObjectstorageV1alpha1().Buckets().List(ctx, metav1.ListOptions{})
	if len(bucketList.Items) > 0 {
		return bucketList
	}
	numtimes := 0
	for numtimes < 10 {
		bucketList, err = client.ObjectstorageV1alpha1().Buckets().List(ctx, metav1.ListOptions{})
		if len(bucketList.Items) >= numExpected {
			return bucketList
		} else {
			glog.Errorf("Failed to fetch the bucket created %v", err)
		}
		numtimes++
		<-time.After(time.Duration(numtimes) * time.Second)
	}
	return &types.BucketList{}
}

// Validates the content of the bucket against bucket request and backet class
// This is used by bucket request unit tests
func ValidateBucket(bucket types.Bucket, bucketrequest types.BucketRequest, bucketclass types.BucketClass) bool {
	if strings.HasPrefix(bucket.Name, bucketrequest.Spec.BucketPrefix) &&
		bucket.Spec.BucketClassName == bucketrequest.Spec.BucketClassName &&
		bucket.Spec.BucketRequest.Name == bucketrequest.Name &&
		bucket.Spec.BucketRequest.Namespace == bucketrequest.Namespace &&
		bucket.Spec.BucketRequest.UID == bucketrequest.ObjectMeta.UID &&
		bucket.Spec.BucketClassName == bucketclass.Name &&
		reflect.DeepEqual(bucket.Spec.Parameters, bucketclass.Parameters) &&
		bucket.Spec.Provisioner == bucketclass.Provisioner &&
		bucket.Spec.RetentionPolicy == bucketclass.RetentionPolicy &&
		bucket.Spec.AnonymousAccessMode == bucketclass.AnonymousAccessMode {
		return true
	}
	return false
}

// Validates the content of the bucket against bucket request and backet class
// This is used by bucket access request unit tests
func GetBucketAccesses(ctx context.Context, client bucketclientset.Interface, numExpected int) *types.BucketAccessList {
	bucketaccessList, _ := client.ObjectstorageV1alpha1().BucketAccesses().List(ctx, metav1.ListOptions{})
	if len(bucketaccessList.Items) > 0 {
		return bucketaccessList
	}
	numtimes := 0
	for numtimes < 10 {
		bucketaccessList, _ = client.ObjectstorageV1alpha1().BucketAccesses().List(ctx, metav1.ListOptions{})
		if len(bucketaccessList.Items) >= numExpected {
			return bucketaccessList
		}
		numtimes++
		<-time.After(time.Duration(numtimes) * time.Second)
	}
	return &types.BucketAccessList{}
}

// Validates the content of the bucket access against bucket access request and backet access class
// This is used by bucket access request unit tests
func ValidateBucketAccess(bucketaccess types.BucketAccess, bucketaccessrequest types.BucketAccessRequest, bucketaccessclass types.BucketAccessClass) bool {
	if bucketaccess.Spec.BucketInstanceName != "" &&
		bucketaccess.Spec.BucketAccessRequest == bucketaccessrequest.Name &&
		bucketaccess.Spec.ServiceAccount == bucketaccessrequest.Spec.ServiceAccountName &&
		bucketaccess.Spec.PolicyActionsConfigMapData != "" &&
		bucketaccess.Spec.Provisioner == bucketaccessclass.Provisioner {
		return true
	}
	return false
}

// Deletes any bucket api object or an array of bucket or bucket access objects.
// This is used by bucket request and bucket access request unit tests
func DeleteObjects(ctx context.Context, client bucketclientset.Interface, objs ...interface{}) {
	for _, obj := range objs {
		switch t := obj.(type) {
		case types.Bucket:
			client.ObjectstorageV1alpha1().Buckets().Delete(ctx, obj.(types.Bucket).Name, metav1.DeleteOptions{})
		case types.BucketRequest:
			client.ObjectstorageV1alpha1().BucketRequests(obj.(types.BucketRequest).Namespace).Delete(ctx, obj.(types.BucketRequest).Name, metav1.DeleteOptions{})
		case types.BucketClass:
			client.ObjectstorageV1alpha1().BucketClasses().Delete(ctx, obj.(types.BucketClass).Name, metav1.DeleteOptions{})
		case []types.Bucket:
			for _, a := range obj.([]types.Bucket) {
				DeleteObjects(ctx, client, a)
			}
		case types.BucketAccess:
			client.ObjectstorageV1alpha1().BucketAccesses().Delete(ctx, obj.(types.BucketAccess).Name, metav1.DeleteOptions{})
		case types.BucketAccessRequest:
			client.ObjectstorageV1alpha1().BucketAccessRequests(obj.(types.BucketAccessRequest).Namespace).Delete(ctx, obj.(types.BucketAccessRequest).Name, metav1.DeleteOptions{})
		case types.BucketAccessClass:
			client.ObjectstorageV1alpha1().BucketAccessClasses().Delete(ctx, obj.(types.BucketAccessClass).Name, metav1.DeleteOptions{})
		case []types.BucketAccess:
			for _, a := range obj.([]types.BucketAccess) {
				DeleteObjects(ctx, client, a)
			}
		default:
			glog.Errorf("Unknown Obj of type %v", t)
		}
	}
}

// CreateBucketRequest creates a bucket request object or return an existing bucket request object
// This is used by bucket request unit tests
func CreateBucketRequest(ctx context.Context, client bucketclientset.Interface, br *types.BucketRequest) (*types.BucketRequest, error) {
	br, err := client.ObjectstorageV1alpha1().BucketRequests(br.Namespace).Create(ctx, br, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		br, err = client.ObjectstorageV1alpha1().BucketRequests(br.Namespace).Get(ctx, br.Name, metav1.GetOptions{})
	}
	return br, err
}

// CreateBucketAccessRequest creates a bucket access request object or return an existing bucket access request object
// This is used by bucket access request unit tests
func CreateBucketAccessRequest(ctx context.Context, client bucketclientset.Interface, bar *types.BucketAccessRequest) (*types.BucketAccessRequest, error) {
	bar, err := client.ObjectstorageV1alpha1().BucketAccessRequests(bar.Namespace).Create(ctx, bar, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		bar, err = client.ObjectstorageV1alpha1().BucketAccessRequests(bar.Namespace).Get(ctx, bar.Name, metav1.GetOptions{})
	}
	return bar, err
}

// CreateBucketClass creates a bucket class object or return an existing bucket class object
// This is used by bucket request unit tests
func CreateBucketClass(ctx context.Context, client bucketclientset.Interface, bc *types.BucketClass) (*types.BucketClass, error) {
	bc, err := client.ObjectstorageV1alpha1().BucketClasses().Create(ctx, bc, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		bc, err = client.ObjectstorageV1alpha1().BucketClasses().Get(ctx, bc.Name, metav1.GetOptions{})
	}
	return bc, err
}

// CreateBucketAccessClass creates a bucket access class object or return an existing bucket access class object
// This is used by bucket request unit tests
func CreateBucketAccessClass(ctx context.Context, client bucketclientset.Interface, bac *types.BucketAccessClass) (*types.BucketAccessClass, error) {
	bac, err := client.ObjectstorageV1alpha1().BucketAccessClasses().Create(ctx, bac, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		bac, err = client.ObjectstorageV1alpha1().BucketAccessClasses().Get(ctx, bac.Name, metav1.GetOptions{})
	}
	return bac, err
}

// CreateBucket creates a bucket object or return an existing bucket object
// This is used by bucket request unit tests
func CreateBucket(ctx context.Context, client bucketclientset.Interface, b *types.Bucket) (*types.Bucket, error) {
	b, err := client.ObjectstorageV1alpha1().Buckets().Create(ctx, b, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		b, err = client.ObjectstorageV1alpha1().Buckets().Get(ctx, b.Name, metav1.GetOptions{})
	}
	return b, err
}

// CreateBucketAccess creates a bucket access object or return an existing bucket access object
// This is used by bucket access request unit tests
func CreateBucketAccess(ctx context.Context, client bucketclientset.Interface, ba *types.BucketAccess) (*types.BucketAccess, error) {
	ba, err := client.ObjectstorageV1alpha1().BucketAccesses().Create(ctx, ba, metav1.CreateOptions{})
	if (err != nil) && apierrors.IsAlreadyExists(err) {
		ba, err = client.ObjectstorageV1alpha1().BucketAccesses().Get(ctx, ba.Name, metav1.GetOptions{})
	}
	return ba, err
}

// getCRDClient returns CRD interface for managing CRD objects programmatically
// Used by unit tests and functional tests
func getCRDClient() (apiextensions.CustomResourceDefinitionInterface, error) {
	config, err := func() (*rest.Config, error) {
		kubeConfig := os.Getenv("KUBECONFIG")

		if kubeConfig != "" {
			return clientcmd.BuildConfigFromFlags("", kubeConfig)
		}
		return rest.InClusterConfig()
	}()

	crdClientset, err := apiextensions.NewForConfig(config)
	if err != nil {
		glog.Fatalf("could not initialize crd client: %v", err)
		return nil, err
	}
	return crdClientset.CustomResourceDefinitions(), err
}

// RegisterCRDs registers the CRD so that unit tests can use the CRD to perform bucket API testing
func RegisterCRDs(ctx context.Context, client apiextensions.CustomResourceDefinitionInterface) error {
	var err error
	if client == nil {
		client, err = getCRDClient()
	}
	if err != nil {
		return err
	}

	roots, err := loader.LoadRoots("github.com/kubernetes-sigs/container-object-storage-interface-api/apis/objectstorage.k8s.io/v1alpha1")
	if err != nil {
		return err
	}

	defn := markers.Must(markers.MakeDefinition("crd", markers.DescribesPackage, crd.Generator{}))
	optionsRegistry := &markers.Registry{}
	if err := optionsRegistry.Register(defn); err != nil {
		return err
	}

	if err := genall.RegisterOptionsMarkers(optionsRegistry); err != nil {
		return err
	}

	if err := crdmarkers.Register(optionsRegistry); err != nil {
		return err
	}

	parser := &crd.Parser{
		Collector: &markers.Collector{
			Registry: optionsRegistry,
		},
		Checker: &loader.TypeChecker{},
	}
	crd.AddKnownTypes(parser)
	for _, root := range roots {
		parser.NeedPackage(root)
	}

	metav1Pkg := crd.FindMetav1(roots)
	if metav1Pkg == nil {
		// no objects in the roots, since nothing imported metav1
		return fmt.Errorf("no objects found in all roots")
	}

	// TODO: allow selecting a specific object
	kubeKinds := crd.FindKubeKinds(parser, metav1Pkg)
	if len(kubeKinds) == 0 {
		// no objects in the roots
		return fmt.Errorf("no kube kind-objects found in all roots")
	}

	//crdClient := utils.GetCRDClient()
	crdClient := client

	for groupKind := range kubeKinds {
		parser.NeedCRDFor(groupKind, func() *int {
			i := 256
			return &i
		}())
		crdRaw := parser.CustomResourceDefinitions[groupKind]
		glog.Infof("creating CRD: %v", groupKind)
		if crdRaw.ObjectMeta.Annotations == nil {
			crdRaw.ObjectMeta.Annotations = map[string]string{}
		}
		if _, ok := crdRaw.ObjectMeta.Annotations[apiextensionsv1.KubeAPIApprovedAnnotation]; !ok {
			crdRaw.ObjectMeta.Annotations[apiextensionsv1.KubeAPIApprovedAnnotation] = "https://github.com/kubernetes/kubernetes/pull/78458"
		}
		if _, err := crdClient.Create(ctx, &crdRaw, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
		}
	}
	return nil
}
