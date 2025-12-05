package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type priceRecord struct {
	name       string
	category   string
	price      float64
	createDate time.Time
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

	csvFiles := extractCSVFiles(data, archiveType)
	if csvFiles == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read archive"})
		return
	}

	var validRecords []priceRecord
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

			name := strings.TrimSpace(record[1])
			category := strings.TrimSpace(record[2])
			if name == "" || category == "" {
				continue
			}

			price, err := strconv.ParseFloat(strings.TrimSpace(record[3]), 64)
			if err != nil || price <= 0 {
				continue
			}

			createDate, err := time.Parse("2006-01-02", strings.TrimSpace(record[4]))
			if err != nil {
				continue
			}

			validRecords = append(validRecords, priceRecord{
				name:       name,
				category:   category,
				price:      price,
				createDate: createDate,
			})
		}
	}

	tx, err := db.Begin(context.Background())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start transaction"})
		return
	}
	defer tx.Rollback(context.Background())

	duplicatesCount := 0
	insertedCount := 0
	categories := make(map[string]bool)
	var totalPrice float64

	for _, rec := range validRecords {
		var exists bool
		err = tx.QueryRow(context.Background(),
			"SELECT EXISTS(SELECT 1 FROM prices WHERE name = $1 AND category = $2 AND price = $3 AND create_date = $4)",
			rec.name, rec.category, rec.price, rec.createDate).Scan(&exists)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}

		if exists {
			duplicatesCount++
			continue
		}

		_, err = tx.Exec(context.Background(),
			"INSERT INTO prices (name, category, price, create_date) VALUES ($1, $2, $3, $4)",
			rec.name, rec.category, rec.price, rec.createDate)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to insert record"})
			return
		}

		insertedCount++
		categories[rec.category] = true
		totalPrice += rec.price
	}

	if err = tx.Commit(context.Background()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit transaction"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"total_count":      len(validRecords),
		"duplicates_count": duplicatesCount,
		"total_items":      insertedCount,
		"total_categories": len(categories),
		"total_price":      totalPrice,
	})
}

func getPrices(c *gin.Context) {
	startDate := c.Query("start")
	endDate := c.Query("end")
	minPrice := c.Query("min")
	maxPrice := c.Query("max")

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

	type priceRow struct {
		id         int
		name       string
		category   string
		price      float64
		createDate time.Time
	}

	var priceRows []priceRow
	for rows.Next() {
		var row priceRow
		err := rows.Scan(&row.id, &row.name, &row.category, &row.price, &row.createDate)
		if err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan row"})
			return
		}
		priceRows = append(priceRows, row)
	}
	rows.Close()

	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "error reading rows"})
		return
	}

	var csvData [][]string
	csvData = append(csvData, []string{"id", "name", "category", "price", "create_date"})
	for _, row := range priceRows {
		csvData = append(csvData, []string{
			strconv.Itoa(row.id),
			row.name,
			row.category,
			strconv.FormatFloat(row.price, 'f', 2, 64),
			row.createDate.Format("2006-01-02"),
		})
	}

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

type csvFileData struct {
	name    string
	content []byte
}

func extractCSVFiles(data []byte, archiveType string) []csvFileData {
	var csvFiles []csvFileData

	if archiveType == "tar" {
		tarReader := tar.NewReader(bytes.NewReader(data))
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil
			}

			if header.Typeflag != tar.TypeReg {
				continue
			}

			fileName := filepath.Base(header.Name)
			if strings.HasPrefix(fileName, "._") {
				continue
			}

			if !strings.HasSuffix(strings.ToLower(header.Name), ".csv") {
				continue
			}

			limitedReader := io.LimitReader(tarReader, header.Size)
			content, err := io.ReadAll(limitedReader)
			if err != nil {
				return nil
			}

			csvFiles = append(csvFiles, csvFileData{name: header.Name, content: content})
		}
	} else {
		zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil
		}

		for _, file := range zipReader.File {
			fileName := filepath.Base(file.Name)
			if strings.HasPrefix(fileName, "._") {
				continue
			}

			if !strings.HasSuffix(strings.ToLower(file.Name), ".csv") {
				continue
			}

			rc, err := file.Open()
			if err != nil {
				return nil
			}

			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil
			}

			csvFiles = append(csvFiles, csvFileData{name: file.Name, content: content})
		}
	}

	return csvFiles
}
