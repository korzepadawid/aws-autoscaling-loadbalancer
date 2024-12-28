package main

import (
	"log"

	"github.com/joho/godotenv"
)

func main() {
	logger := log.Default()
	if err := godotenv.Load(".env"); err != nil {
		logger.Fatalf("Error loading .env file: %v", err)
	}
	logger.Println("Environment variables loaded successfully")
}
