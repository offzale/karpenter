/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/patrickmn/go-cache"

	awsv1alpha1 "github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/amifamily"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/functional"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/project"

	k8sClient "sigs.k8s.io/controller-runtime/pkg/client"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/transport"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
)

const (
	// CacheTTL restricts QPS to AWS APIs to this interval for verifying setup
	// resources. This value represents the maximum eventual consistency between
	// AWS actual state and the controller's ability to provision those
	// resources. Cache hits enable faster provisioning and reduced API load on
	// AWS APIs, which can have a serious impact on performance and scalability.
	// DO NOT CHANGE THIS VALUE WITHOUT DUE CONSIDERATION
	CacheTTL = 60 * time.Second
	// CacheCleanupInterval triggers cache cleanup (lazy eviction) at this interval.
	CacheCleanupInterval = 10 * time.Minute
	// MaxInstanceTypes defines the number of instance type options to pass to CreateFleet
	MaxInstanceTypes = 20
)

func init() {
	v1alpha5.NormalizedLabels = functional.UnionStringMaps(v1alpha5.NormalizedLabels, map[string]string{"topology.ebs.csi.aws.com/zone": v1.LabelTopologyZone})
}

type CloudProvider struct {
	instanceTypeProvider *InstanceTypeProvider
	subnetProvider       *SubnetProvider
	instanceProvider     *InstanceProvider
	kubeClient           k8sClient.Client
}

func NewCloudProvider(ctx context.Context, options cloudprovider.Options) *CloudProvider {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("aws"))
	sess := withUserAgent(session.Must(session.NewSession(
		request.WithRetryer(
			&aws.Config{STSRegionalEndpoint: endpoints.RegionalSTSEndpoint},
			client.DefaultRetryer{NumMaxRetries: client.DefaultRetryerMaxNumRetries},
		),
	)))
	if *sess.Config.Region == "" {
		logging.FromContext(ctx).Debug("AWS region not configured, asking EC2 Instance Metadata Service")
		*sess.Config.Region = getRegionFromIMDS(sess)
	}
	logging.FromContext(ctx).Debugf("Using AWS region %s", *sess.Config.Region)

	ec2api := ec2.New(sess)
	if err := checkEC2Connectivity(ec2api); err != nil {
		logging.FromContext(ctx).Errorf("Checking EC2 API connectivity, %s", err)
	}
	subnetProvider := NewSubnetProvider(ec2api)
	pricingProvider := NewPricingProvider(ctx, NewPricingAPI(sess, *sess.Config.Region), ec2api, *sess.Config.Region,
		injection.GetOptions(ctx).AWSIsolatedVPC, options.StartAsync)
	instanceTypeProvider := NewInstanceTypeProvider(ec2api, subnetProvider, pricingProvider)
	return &CloudProvider{
		instanceTypeProvider: instanceTypeProvider,
		subnetProvider:       subnetProvider,
		instanceProvider: NewInstanceProvider(ctx, ec2api, instanceTypeProvider, subnetProvider,
			NewLaunchTemplateProvider(
				ctx,
				ec2api,
				options.ClientSet,
				amifamily.New(ctx, ssm.New(sess), ec2api, cache.New(CacheTTL, CacheCleanupInterval), cache.New(CacheTTL, CacheCleanupInterval), options.KubeClient),
				NewSecurityGroupProvider(ec2api),
				getCABundle(ctx),
				options.StartAsync,
			),
		),
		kubeClient: options.KubeClient,
	}
}

// checkEC2Connectivity makes a dry-run call to DescribeInstanceTypes.  If it fails, we provide an early indicator that we
// are having issues connecting to the EC2 API.
func checkEC2Connectivity(api *ec2.EC2) error {
	_, err := api.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{DryRun: aws.Bool(true)})
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "DryRunOperation" {
		return nil
	}
	return err
}

// Create a node given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeRequest *cloudprovider.NodeRequest) (*v1.Node, error) {
	aws, err := c.getProvider(ctx, nodeRequest.Template.Provider, nodeRequest.Template.ProviderRef)
	if err != nil {
		return nil, err
	}
	return c.instanceProvider.Create(ctx, aws, nodeRequest)
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, provisioner *v1alpha5.Provisioner) ([]cloudprovider.InstanceType, error) {
	aws, err := c.getProvider(ctx, provisioner.Spec.Provider, provisioner.Spec.ProviderRef)
	if err != nil {
		return nil, err
	}
	return c.instanceTypeProvider.Get(ctx, aws)
}

func (c *CloudProvider) Delete(ctx context.Context, node *v1.Node) error {
	return c.instanceProvider.Terminate(ctx, node)
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "aws"
}

// get the current region from EC2 IMDS
func getRegionFromIMDS(sess *session.Session) string {
	region, err := ec2metadata.New(sess).Region()
	if err != nil {
		panic(fmt.Sprintf("Failed to call the metadata server's region API, %s", err))
	}
	return region
}

// withUserAgent adds a karpenter specific user-agent string to AWS session
func withUserAgent(sess *session.Session) *session.Session {
	userAgent := fmt.Sprintf("karpenter.sh-%s", project.Version)
	sess.Handlers.Build.PushBack(request.MakeAddToUserAgentFreeFormHandler(userAgent))
	return sess
}

func getCABundle(ctx context.Context) *string {
	// Discover CA Bundle from the REST client. We could alternatively
	// have used the simpler client-go InClusterConfig() method.
	// However, that only works when Karpenter is running as a Pod
	// within the same cluster it's managing.
	restConfig := injection.GetConfig(ctx)
	if restConfig == nil {
		return nil
	}
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		logging.FromContext(ctx).Fatalf("Unable to discover caBundle, loading transport config, %v", err)
		return nil
	}
	_, err = transport.TLSConfigFor(transportConfig) // fills in CAData!
	if err != nil {
		logging.FromContext(ctx).Fatalf("Unable to discover caBundle, loading TLS config, %v", err)
		return nil
	}
	logging.FromContext(ctx).Debugf("Discovered caBundle, length %d", len(transportConfig.TLS.CAData))
	return ptr.String(base64.StdEncoding.EncodeToString(transportConfig.TLS.CAData))
}

func (c *CloudProvider) getProvider(ctx context.Context, provider *runtime.RawExtension, providerRef *v1alpha5.ProviderRef) (*v1alpha1.AWS, error) {
	if providerRef != nil {
		var ant awsv1alpha1.AWSNodeTemplate
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: providerRef.Name}, &ant); err != nil {
			return nil, fmt.Errorf("getting providerRef, %w", err)
		}
		return &ant.Spec.AWS, nil
	}
	aws, err := v1alpha1.Deserialize(provider)
	if err != nil {
		return nil, err
	}
	return aws, nil
}
