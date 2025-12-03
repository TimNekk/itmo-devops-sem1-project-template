package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

var db *pgx.Conn

func connectDB() {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Fatalf("DATABASE_URL is not set")
	}

	var err error
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		db, err = pgx.Connect(context.Background(), connStr)
		if err == nil {
			log.Printf("Successfully connected to database")
			break
		}
		log.Printf("Failed to connect to database (attempt %d/%d): %v", i+1, maxRetries, err)
		if i < maxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	if err != nil {
		log.Fatalf("Unable to connect to database after %d attempts: %v", maxRetries, err)
	}
}

func initDB() error {
	query := `
	CREATE TABLE IF NOT EXISTS prices (
		id INTEGER NOT NULL,
		name VARCHAR(255) NOT NULL,
		category VARCHAR(255) NOT NULL,
		price DECIMAL(10, 2) NOT NULL,
		create_date DATE NOT NULL,
		PRIMARY KEY (id)
	);
	`
	_, err := db.Exec(context.Background(), query)
	return err
}

func closeDB() {
	db.Close(context.Background())
}

