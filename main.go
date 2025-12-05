package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
)

func main() {
	if err := run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func run() error {
	connectDB()
	defer closeDB()

	if err := initDB(); err != nil {
		return err
	}

	r := gin.Default()
	r.POST("/api/v0/prices", uploadPrices)
	r.GET("/api/v0/prices", getPrices)

	return r.Run()
}
