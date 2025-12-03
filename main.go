package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var db *pgx.Conn

func main() {
	// Initialize database connection
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Fatalf("DATABASE_URL is not set")
	}

	// Retry connection with backoff
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
	defer db.Close(context.Background())

	// Create table if not exists
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

func uploadPrices(c *gin.Context) {
	archiveType := c.Query("type")
	if archiveType == "" {
		archiveType = "zip"
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no file uploaded"})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to open uploaded file"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}

	totalCount := 0
	duplicatesCount := 0
	seenIDs := make(map[int]bool)
	categories := make(map[string]bool)

	// Parse all CSV files from archive
	var recordsToInsert []map[string]string
	var csvFiles []struct {
		name    string
		content []byte
	}

	if archiveType == "tar" {
		// Handle TAR archive
		tarReader := tar.NewReader(bytes.NewReader(data))
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read tar archive"})
				return
			}

			if header.Typeflag != tar.TypeReg {
				continue
			}

			// Skip macOS resource fork files
			fileName := filepath.Base(header.Name)
			if strings.HasPrefix(fileName, "._") {
				continue
			}

			if !strings.HasSuffix(strings.ToLower(header.Name), ".csv") {
				continue
			}

			// Read file content, limiting to the file size from header
			limitedReader := io.LimitReader(tarReader, header.Size)
			content, err := io.ReadAll(limitedReader)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("unable to read file %s in tar: %v", header.Name, err)})
				return
			}

			csvFiles = append(csvFiles, struct {
				name    string
				content []byte
			}{name: header.Name, content: content})
		}
	} else {
		// Handle ZIP archive (default)
		zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create zip reader"})
			return
		}

		for _, file := range zipReader.File {
			// Skip macOS resource fork files
			fileName := filepath.Base(file.Name)
			if strings.HasPrefix(fileName, "._") {
				continue
			}

			if !strings.HasSuffix(strings.ToLower(file.Name), ".csv") {
				continue
			}

			rc, err := file.Open()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to open file in zip"})
				return
			}

			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file in zip"})
				return
			}

			csvFiles = append(csvFiles, struct {
				name    string
				content []byte
			}{name: file.Name, content: content})
		}
	}

	// Process CSV files
	for _, csvFile := range csvFiles {
		csvReader := csv.NewReader(bytes.NewReader(csvFile.content))
		csvRecords, err := csvReader.ReadAll()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("unable to read csv file %s: %v", csvFile.name, err)})
			return
		}

		if len(csvRecords) == 0 {
			continue
		}

		for i, record := range csvRecords {
			if i == 0 {
				continue
			}

			if len(record) < 5 {
				continue
			}

			totalCount++

			id, err := strconv.Atoi(strings.TrimSpace(record[0]))
			if err != nil {
				continue
			}

			price, err := strconv.ParseFloat(strings.TrimSpace(record[3]), 64)
			if err != nil || price <= 0 {
				continue
			}

			_, err = time.Parse("2006-01-02", strings.TrimSpace(record[4]))
			if err != nil {
				continue
			}

			// Validate that name and category are not empty
			name := strings.TrimSpace(record[1])
			category := strings.TrimSpace(record[2])
			if name == "" || category == "" {
				continue
			}

			if seenIDs[id] {
				duplicatesCount++
				continue
			}

			var exists bool
			err = db.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM prices WHERE id = $1)", id).Scan(&exists)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
				return
			}
			if exists {
				duplicatesCount++
				continue
			}

			seenIDs[id] = true
			categories[category] = true

			recordsToInsert = append(recordsToInsert, map[string]string{
				"id":          record[0],
				"name":        name,
				"category":    category,
				"price":       record[3],
				"create_date": strings.TrimSpace(record[4]),
			})
		}
	}

	for _, record := range recordsToInsert {
		id, _ := strconv.Atoi(record["id"])
		price, _ := strconv.ParseFloat(record["price"], 64)
		_, err := db.Exec(context.Background(),
			"INSERT INTO prices (id, name, category, price, create_date) VALUES ($1, $2, $3, $4, $5)",
			id, record["name"], record["category"], price, record["create_date"])
		if err != nil {
			duplicatesCount++
		}
	}

	var totalItems int
	var totalCategories int
	var totalPrice float64

	err = db.QueryRow(context.Background(), "SELECT COUNT(*), COUNT(DISTINCT category), COALESCE(SUM(price), 0) FROM prices").Scan(&totalItems, &totalCategories, &totalPrice)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to calculate statistics"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"total_count":      totalCount,
		"duplicates_count": duplicatesCount,
		"total_items":      totalItems,
		"total_categories": totalCategories,
		"total_price":      totalPrice,
	})
}

func getPrices(c *gin.Context) {
	startDate := c.Query("start")
	endDate := c.Query("end")
	minPrice := c.Query("min")
	maxPrice := c.Query("max")

	// Build query
	query := "SELECT id, name, category, price, create_date FROM prices WHERE 1=1"
	args := []interface{}{}
	argIndex := 1

	if startDate != "" {
		query += fmt.Sprintf(" AND create_date >= $%d", argIndex)
		args = append(args, startDate)
		argIndex++
	}

	if endDate != "" {
		query += fmt.Sprintf(" AND create_date <= $%d", argIndex)
		args = append(args, endDate)
		argIndex++
	}

	if minPrice != "" {
		query += fmt.Sprintf(" AND price >= $%d", argIndex)
		args = append(args, minPrice)
		argIndex++
	}

	if maxPrice != "" {
		query += fmt.Sprintf(" AND price <= $%d", argIndex)
		args = append(args, maxPrice)
		argIndex++
	}

	query += " ORDER BY id"

	rows, err := db.Query(context.Background(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database query failed"})
		return
	}
	defer rows.Close()

	var csvData [][]string
	csvData = append(csvData, []string{"id", "name", "category", "price", "create_date"})

	for rows.Next() {
		var id int
		var name, category string
		var price float64
		var createDate time.Time

		err := rows.Scan(&id, &name, &category, &price, &createDate)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan row"})
			return
		}

		csvData = append(csvData, []string{
			strconv.Itoa(id),
			name,
			category,
			strconv.FormatFloat(price, 'f', 2, 64),
			createDate.Format("2006-01-02"),
		})
	}

	// Create zip archive
	var zipBuffer bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuffer)

	csvFile, err := zipWriter.Create("data.csv")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create csv file in zip"})
		return
	}

	csvWriter := csv.NewWriter(csvFile)
	for _, record := range csvData {
		if err := csvWriter.Write(record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write csv data"})
			return
		}
	}
	csvWriter.Flush()

	if err := zipWriter.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to close zip writer"})
		return
	}

	c.Data(http.StatusOK, "application/zip", zipBuffer.Bytes())
}
