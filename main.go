package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

const (
	ENV_FILE_PATH    = ".env"
	USER_DATA_SCRIPT = "user_data.sh"

	AWS_REGION                 = "us-east-1"
	AWS_AMI_ID                 = "ami-01816d07b1128cd2d" // Amazon Linux 2023 AMI
	AWS_LAUNCH_TEMPLATE_PREFIX = "webservice-launch-template-"
)

func main() {
	logger := log.Default()
	if err := godotenv.Load(ENV_FILE_PATH); err != nil {
		logger.Fatalf("Error loading .env file: %v", err)
	}
	logger.Println("Environment variables loaded successfully")

	userDataBytes, err := os.ReadFile(USER_DATA_SCRIPT)
	if err != nil {
		logger.Fatalf("Error reading user_data.sh file: %v", err)
	}
	logger.Println("user_data.sh file read successfully")

	ctx, cancelFunc := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFunc()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithDefaultRegion(AWS_REGION))
	if err != nil {
		log.Fatal(err)
	}
	logger.Println("AWS configuration loaded successfully")

	ec2Client := ec2.NewFromConfig(cfg)
	base64UserData := base64.StdEncoding.EncodeToString(userDataBytes)
	ec2LaunchTemplate, err := ec2Client.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateData: &types.RequestLaunchTemplateData{
			UserData:     aws.String(base64UserData),
			ImageId:      aws.String(AWS_AMI_ID),
			InstanceType: types.InstanceTypeT2Micro,
		},
		LaunchTemplateName: aws.String(AWS_LAUNCH_TEMPLATE_PREFIX + uuid.NewString()),
	})
	if err != nil {
		logger.Fatalf("Error creating launch template: %v", err)
	}
	logger.Printf("Launch template created with ID: %s", *ec2LaunchTemplate.LaunchTemplate.LaunchTemplateId)
}
