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

	var recordsToInsert []map[string]string
	csvFiles := extractCSVFiles(data, archiveType)
	if csvFiles == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read archive"})
		return
	}

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

