package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingTypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbTypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

const (
	EnvFilePath    = ".env"
	UserDataScript = "user_data.sh"

	AWSRegion                   = "us-east-1"
	AWSAmiID                    = "ami-01816d07b1128cd2d" // Amazon Linux 2023 AMI
	AWSLaunchTemplatePrefix     = "webservice-launch-template-"
	AWSLaunchTemplateVersion    = "$Latest"
	AWSSecurityGroupPrefix      = "webservice-sg-"
	AWSAutoscalingGroupPrefix   = "webservice-sg-"
	AWSAutoscalingPolicyPrefix  = "webservice-sg-"
	AWSSecurityGroupDescription = "Security group for port 8080 access"
	AWSAutoscalingPolicyType    = "TargetTrackingScaling"
	AWSMinEC2Count              = 2
	AWSMaxEC2Count              = 5

	AWSAutoScalingCPUThreshold = 30.0
)

var (
	AWSSubnetAvailabilityZones = map[string]string{
		"10.0.1.0/24": "us-east-1a",
		"10.0.2.0/24": "us-east-1b",
	}
)

func main() {
	logger := log.Default()
	if err := godotenv.Load(EnvFilePath); err != nil {
		logger.Fatalf("Error loading .env file: %v", err)
	}
	logger.Println("Environment variables loaded successfully")

	ctx, cancelFunc := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancelFunc()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithDefaultRegion(AWSRegion))
	if err != nil {
		log.Fatal(err)
	}
	logger.Println("AWS configuration loaded successfully")
	ec2Client := ec2.NewFromConfig(cfg)
	elbClient := elasticloadbalancingv2.NewFromConfig(cfg)
	autoscalingClient := autoscaling.NewFromConfig(cfg)

	vpcID, err := CreateVPC(ctx, logger, ec2Client)
	if err != nil {
		logger.Fatal(err)
	}

	internetGatewayID, err := CreateInternetGateway(ctx, logger, ec2Client, vpcID)
	if err != nil {
		logger.Fatal(err)
	}

	subnetIDs, err := CreateSubnets(ctx, logger, ec2Client, vpcID, internetGatewayID)
	if err != nil {
		logger.Fatal(err)
	}

	securityGroupID, err := CreateSecurityGroup(ctx, logger, ec2Client, vpcID)
	if err != nil {
		logger.Fatal(err)
	}

	launchTemplateID, err := CreateLaunchTemplate(ctx, logger, ec2Client, securityGroupID)
	if err != nil {
		logger.Fatal(err)
	}

	targetGroupARN, err := CreateTargetGroup(ctx, logger, elbClient, vpcID)
	if err != nil {
		logger.Fatal(err)
	}

	if err := CreateAutoscalingGroup(ctx, logger, autoscalingClient, launchTemplateID, targetGroupARN, subnetIDs); err != nil {
		logger.Fatal(err)
	}

	loadBalancerARN, err := CreateLoadBalancer(ctx, logger, elbClient, subnetIDs, securityGroupID)
	if err != nil {
		logger.Fatal(err)
	}

	if err = CreateListener(ctx, logger, elbClient, loadBalancerARN, targetGroupARN); err != nil {
		logger.Fatal(err)
	}

	logger.Println("All AWS resources created successfully")
}

func CreateVPC(ctx context.Context, logger *log.Logger, ec2Client *ec2.Client) (string, error) {
	result, err := ec2Client.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	})
	if err != nil {
		return "", fmt.Errorf("error creating VPC: %w", err)
	}
	logger.Printf("VPC created with ID: %s", *result.Vpc.VpcId)

	modifyVPC := &ec2.ModifyVpcAttributeInput{
		VpcId: result.Vpc.VpcId,
		EnableDnsHostnames: &types.AttributeBooleanValue{
			Value: aws.Bool(true),
		},
	}
	if _, err = ec2Client.ModifyVpcAttribute(ctx, modifyVPC); err != nil {
		return "", fmt.Errorf("error enabling DNS hostnames: %w", err)
	}
	logger.Printf("DNS hostnames enabled for VPC with ID: %s", *result.Vpc.VpcId)

	return *result.Vpc.VpcId, nil
}

func CreateSubnets(
	ctx context.Context,
	logger *log.Logger,
	ec2Client *ec2.Client,
	vpcID string,
	internetGatewayID string,
) ([]string, error) {
	subnets := make([]string, 0, len(AWSSubnetAvailabilityZones))

	routeTableResult, err := ec2Client.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
	})
	if err != nil {
		return nil, fmt.Errorf("error creating route table: %w", err)
	}
	routeTableID := *routeTableResult.RouteTable.RouteTableId
	logger.Printf("Route table created with ID: %s", routeTableID)

	if _, err = ec2Client.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(routeTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(internetGatewayID),
	}); err != nil {
		return nil, fmt.Errorf("error creating route to internet gateway: %w", err)
	}
	logger.Printf("Created route to Internet Gateway %s in route table %s", internetGatewayID, routeTableID)

	for cidrBlock, availabilityZone := range AWSSubnetAvailabilityZones {
		subnetResult, err := ec2Client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
			VpcId:            aws.String(vpcID),
			CidrBlock:        aws.String(cidrBlock),
			AvailabilityZone: aws.String(availabilityZone),
		})
		if err != nil {
			return nil, fmt.Errorf("error creating subnet: %w", err)
		}
		subnetID := *subnetResult.Subnet.SubnetId
		logger.Printf("Subnet created with ID: %s", subnetID)

		if _, err := ec2Client.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(subnetID),
			MapPublicIpOnLaunch: &types.AttributeBooleanValue{Value: aws.Bool(true)},
		}); err != nil {
			return nil, fmt.Errorf("error enabling auto-assign public IPv4: %w", err)
		}
		logger.Printf("Enabled auto-assign public IPv4 for subnet: %s", subnetID)

		if _, err = ec2Client.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(routeTableID),
			SubnetId:     aws.String(subnetID),
		}); err != nil {
			return nil, fmt.Errorf("error associating route table: %w", err)
		}
		logger.Printf("Associated route table %s with subnet %s", routeTableID, subnetID)

		subnets = append(subnets, subnetID)
	}

	return subnets, nil
}

func CreateSecurityGroup(ctx context.Context, logger *log.Logger, ec2Client *ec2.Client, vpcID string) (string, error) {
	sgName := AWSSecurityGroupPrefix + uuid.NewString()
	createOutput, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String(AWSSecurityGroupDescription),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		return "", fmt.Errorf("error creating security group: %w", err)
	}
	logger.Printf("Created security group with ID: %s", *createOutput.GroupId)

	ec2IngressInput := &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: createOutput.GroupId,
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(8080),
				ToPort:     aws.Int32(8080),
				IpRanges: []types.IpRange{
					{
						CidrIp: aws.String("0.0.0.0/0"),
					},
				},
			},
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(80),
				ToPort:     aws.Int32(80),
				IpRanges: []types.IpRange{
					{
						CidrIp: aws.String("0.0.0.0/0"),
					},
				},
			},
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(443),
				ToPort:     aws.Int32(443),
				IpRanges: []types.IpRange{
					{
						CidrIp: aws.String("0.0.0.0/0"),
					},
				},
			},
		},
	}

	if _, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, ec2IngressInput); err != nil {
		return "", fmt.Errorf("error adding inbound (ingress) rule for port 8080: %w", err)
	}
	logger.Printf("Added inbound (ingress) rule for port 8080 to security group with ID: %s", *createOutput.GroupId)

	return *createOutput.GroupId, nil
}

func CreateLaunchTemplate(ctx context.Context, logger *log.Logger, ec2Client *ec2.Client, securityGroupID string) (string, error) {
	userDataBytes, err := os.ReadFile(UserDataScript)
	if err != nil {
		return "", fmt.Errorf("error reading user_data.sh file: %w", err)
	}
	logger.Println("user_data.sh file read successfully")

	base64UserData := base64.StdEncoding.EncodeToString(userDataBytes)
	ec2LaunchTemplate, err := ec2Client.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateData: &types.RequestLaunchTemplateData{
			UserData:     aws.String(base64UserData),
			ImageId:      aws.String(AWSAmiID),
			InstanceType: types.InstanceTypeT2Micro,
			SecurityGroupIds: []string{
				securityGroupID,
			},
		},
		LaunchTemplateName: aws.String(AWSLaunchTemplatePrefix + uuid.NewString()),
	})
	if err != nil {
		return "", fmt.Errorf("error creating launch template: %w", err)
	}
	logger.Printf("Launch template created with ID: %s", *ec2LaunchTemplate.LaunchTemplate.LaunchTemplateId)

	return *ec2LaunchTemplate.LaunchTemplate.LaunchTemplateId, nil
}

func CreateInternetGateway(ctx context.Context, logger *log.Logger, ec2Client *ec2.Client, vpcID string) (string, error) {
	result, err := ec2Client.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	if err != nil {
		return "", fmt.Errorf("error creating internet gateway: %w", err)
	}
	logger.Printf("Internet gateway created with ID: %s", *result.InternetGateway.InternetGatewayId)

	if _, err = ec2Client.AttachInternetGateway(context.TODO(), &ec2.AttachInternetGatewayInput{
		InternetGatewayId: result.InternetGateway.InternetGatewayId,
		VpcId:             aws.String(vpcID),
	}); err != nil {
		return "", fmt.Errorf("error attaching internet gateway to VPC: %w", err)
	}

	logger.Printf("Internet gateway %s attached to VPC with ID: %s", *result.InternetGateway.InternetGatewayId, vpcID)

	return *result.InternetGateway.InternetGatewayId, nil
}

func CreateLoadBalancer(ctx context.Context, logger *log.Logger, elbClient *elasticloadbalancingv2.Client, subnetIDs []string, securityGroupID string) (string, error) {
	input := &elasticloadbalancingv2.CreateLoadBalancerInput{
		Name:           aws.String("webservice-load-balancer"),
		Scheme:         elbTypes.LoadBalancerSchemeEnumInternetFacing,
		Subnets:        subnetIDs,
		SecurityGroups: []string{securityGroupID},
		IpAddressType:  elbTypes.IpAddressTypeIpv4,
		Type:           elbTypes.LoadBalancerTypeEnumApplication,
	}

	output, err := elbClient.CreateLoadBalancer(ctx, input)
	if err != nil {
		return "", fmt.Errorf("error creating load balancer: %w", err)
	}

	lbARN := *output.LoadBalancers[0].LoadBalancerArn
	logger.Printf("Load balancer created with ARN: %s", lbARN)

	return lbARN, nil
}

func CreateTargetGroup(ctx context.Context, logger *log.Logger, elbClient *elasticloadbalancingv2.Client, vpcID string) (string, error) {
	input := &elasticloadbalancingv2.CreateTargetGroupInput{
		Name:       aws.String("webservice-target-group"),
		Protocol:   elbTypes.ProtocolEnumHttp,
		Port:       aws.Int32(8080),
		VpcId:      aws.String(vpcID),
		TargetType: elbTypes.TargetTypeEnumInstance,
	}

	output, err := elbClient.CreateTargetGroup(ctx, input)
	if err != nil {
		return "", fmt.Errorf("error creating target group: %w", err)
	}

	tgARN := *output.TargetGroups[0].TargetGroupArn
	logger.Printf("Target group created with ARN: %s", tgARN)

	return tgARN, nil
}

func CreateListener(ctx context.Context, logger *log.Logger, elbClient *elasticloadbalancingv2.Client, loadBalancerARN, targetGroupARN string) error {
	input := &elasticloadbalancingv2.CreateListenerInput{
		LoadBalancerArn: aws.String(loadBalancerARN),
		Protocol:        elbTypes.ProtocolEnumHttp,
		Port:            aws.Int32(80),
		DefaultActions: []elbTypes.Action{
			{
				Type: elbTypes.ActionTypeEnumForward,
				ForwardConfig: &elbTypes.ForwardActionConfig{
					TargetGroups: []elbTypes.TargetGroupTuple{
						{
							TargetGroupArn: aws.String(targetGroupARN),
						},
					},
				},
			},
		},
	}

	if _, err := elbClient.CreateListener(ctx, input); err != nil {
		return fmt.Errorf("error creating listener: %w", err)
	}

	logger.Println("Listener created successfully")
	return nil
}

func CreateAutoscalingGroup(ctx context.Context, logger *log.Logger, autoscalingClient *autoscaling.Client, launchTemplateID string, targetGroupARN string, subnetIDs []string) error {
	autoscalingGroupName := AWSAutoscalingGroupPrefix + uuid.NewString()
	if _, err := autoscalingClient.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(autoscalingGroupName),
		LaunchTemplate: &autoscalingTypes.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(launchTemplateID),
			Version:          aws.String(AWSLaunchTemplateVersion),
		},
		MinSize: aws.Int32(AWSMinEC2Count),
		MaxSize: aws.Int32(AWSMaxEC2Count),
		TargetGroupARNs: []string{
			targetGroupARN,
		},
		VPCZoneIdentifier: aws.String(strings.Join(subnetIDs, ",")),
	}); err != nil {
		return fmt.Errorf("error creating autoscaling group: %w", err)
	}
	logger.Printf("Autoscaling group created with name: %s", autoscalingGroupName)

	policyInput := &autoscaling.PutScalingPolicyInput{
		AutoScalingGroupName: aws.String(autoscalingGroupName),
		PolicyName:           aws.String(AWSAutoscalingPolicyPrefix + uuid.NewString()),
		PolicyType:           aws.String(AWSAutoscalingPolicyType),
		TargetTrackingConfiguration: &autoscalingTypes.TargetTrackingConfiguration{
			TargetValue: aws.Float64(AWSAutoScalingCPUThreshold),
			PredefinedMetricSpecification: &autoscalingTypes.PredefinedMetricSpecification{
				PredefinedMetricType: autoscalingTypes.MetricTypeASGAverageCPUUtilization,
			},
		},
	}
	if _, err := autoscalingClient.PutScalingPolicy(ctx, policyInput); err != nil {
		return fmt.Errorf("error creating autoscaling policy: %w", err)
	}
	logger.Println("Autoscaling policy created successfully")

	return nil
}
