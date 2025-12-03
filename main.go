package main

import (
	"log"

	"github.com/gin-gonic/gin"
)

func main() {
	connectDB()
	defer closeDB()

	if err := initDB(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	r := gin.Default()
	r.POST("/api/v0/prices", uploadPrices)
	r.GET("/api/v0/prices", getPrices)

	if err := r.Run(); err != nil {
		log.Fatalf("r.Run() failed: %v", err)
	}
}
