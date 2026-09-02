// Render deployment update
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type Point struct {
	X float64
	Y float64
	Z float64
}

type AnalyzeResponse struct {
	Success               bool    `json:"success"`
	VolumeCm3             float64 `json:"volume_cm3"`
	SolidWeightG          float64 `json:"solid_weight_g"`
	EstimatedPrintWeightG float64 `json:"estimated_print_weight_g"`
	TriangleCount         uint32  `json:"triangle_count"`
	Material              string  `json:"material"`
	Infill                float64 `json:"infill"`
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", healthHandler)
	mux.HandleFunc("/app", appHandler)
	mux.HandleFunc("/analyze", analyzeHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Println("STL Analysis API running on port", port)

	err := http.ListenAndServe(":"+port, corsMiddleware(mux))
	if err != nil {
		panic(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "STL Analysis API is running",
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set(
			"Access-Control-Allow-Methods",
			"POST, GET, OPTIONS",
		)
		w.Header().Set(
			"Access-Control-Allow-Headers",
			"Content-Type",
		)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func analyzeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendError(
			w,
			http.StatusMethodNotAllowed,
			"Only POST requests are allowed",
		)
		return
	}

	// Maximum upload size: 50 MB
	r.Body = http.MaxBytesReader(
		w,
		r.Body,
		50<<20,
	)

	err := r.ParseMultipartForm(50 << 20)
	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"File is too large or form is invalid",
		)
		return
	}

	// Shopify frontend sends the STL as "file"
	file, header, err := r.FormFile("file")
	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"STL file is required",
		)
		return
	}

	defer file.Close()

	if !strings.HasSuffix(
		strings.ToLower(header.Filename),
		".stl",
	) {
		sendError(
			w,
			http.StatusBadRequest,
			"Only STL files are allowed",
		)
		return
	}

	fileData, err := io.ReadAll(file)
	if err != nil {
		sendError(
			w,
			http.StatusInternalServerError,
			"Could not read STL file",
		)
		return
	}

	if len(fileData) == 0 {
		sendError(
			w,
			http.StatusBadRequest,
			"STL file is empty",
		)
		return
	}

	// Material from Shopify
	material := strings.ToUpper(
		strings.TrimSpace(
			r.FormValue("material"),
		),
	)

	if material == "" {
		material = "PLA"
	}

	// Infill from Shopify
	infill := 20.0

	if infillString := r.FormValue("infill"); infillString != "" {
		parsedInfill, err := strconv.ParseFloat(
			infillString,
			64,
		)

		if err == nil {
			infill = parsedInfill
		}
	}

	if infill < 0 {
		infill = 0
	}

	if infill > 100 {
		infill = 100
	}

	// Calculate STL volume
	volumeMM3, triangleCount, err := calculateSTLVolume(
		fileData,
	)

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"Invalid or unsupported STL file: "+err.Error(),
		)
		return
	}

	// Convert mm³ → cm³
	volumeCm3 := volumeMM3 / 1000.0

	// Material density
	density := getMaterialDensity(material)

	// Solid object weight
	solidWeight := volumeCm3 * density

	// Estimated printable weight
	estimatedWeight := estimatePrintWeight(
		solidWeight,
		infill,
	)

	response := AnalyzeResponse{
		Success:               true,
		VolumeCm3:             round(volumeCm3, 2),
		SolidWeightG:          round(solidWeight, 2),
		EstimatedPrintWeightG: round(estimatedWeight, 2),
		TriangleCount:         triangleCount,
		Material:              material,
		Infill:                infill,
	}

	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	json.NewEncoder(w).Encode(response)
}

func calculateSTLVolume(
	data []byte,
) (float64, uint32, error) {
	if isBinarySTL(data) {
		return calculateBinarySTLVolume(data)
	}

	return calculateASCIISTLVolume(data)
}

func isBinarySTL(data []byte) bool {
	if len(data) < 84 {
		return false
	}

	triangleCount := binary.LittleEndian.Uint32(
		data[80:84],
	)

	expectedSize := 84 + int(triangleCount)*50

	return len(data) == expectedSize
}

func calculateBinarySTLVolume(
	data []byte,
) (float64, uint32, error) {
	if len(data) < 84 {
		return 0, 0, fmt.Errorf(
			"binary STL file is too small",
		)
	}

	triangleCount := binary.LittleEndian.Uint32(
		data[80:84],
	)

	expectedSize := 84 + int(triangleCount)*50

	if len(data) < expectedSize {
		return 0, 0, fmt.Errorf(
			"binary STL file is incomplete",
		)
	}

	totalVolume := 0.0
	offset := 84

	for i := uint32(0); i < triangleCount; i++ {
		// Skip normal vector
		offset += 12

		p1 := readBinaryPoint(data[offset:])
		offset += 12

		p2 := readBinaryPoint(data[offset:])
		offset += 12

		p3 := readBinaryPoint(data[offset:])
		offset += 12

		// Skip attribute byte count
		offset += 2

		totalVolume += signedTriangleVolume(
			p1,
			p2,
			p3,
		)
	}

	return math.Abs(totalVolume), triangleCount, nil
}

func readBinaryPoint(data []byte) Point {
	return Point{
		X: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(data[0:4]),
			),
		),
		Y: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(data[4:8]),
			),
		),
		Z: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(data[8:12]),
			),
		),
	}
}

func calculateASCIISTLVolume(
	data []byte,
) (float64, uint32, error) {
	lines := bytes.Split(data, []byte("\n"))

	vertices := make([]Point, 0)
	totalVolume := 0.0
	triangleCount := uint32(0)

	for _, lineBytes := range lines {
		line := strings.TrimSpace(
			string(lineBytes),
		)

		if !strings.HasPrefix(
			strings.ToLower(line),
			"vertex",
		) {
			continue
		}

		fields := strings.Fields(line)

		if len(fields) != 4 {
			continue
		}

		x, err1 := strconv.ParseFloat(
			fields[1],
			64,
		)

		y, err2 := strconv.ParseFloat(
			fields[2],
			64,
		)

		z, err3 := strconv.ParseFloat(
			fields[3],
			64,
		)

		if err1 != nil ||
			err2 != nil ||
			err3 != nil {
			continue
		}

		vertices = append(
			vertices,
			Point{
				X: x,
				Y: y,
				Z: z,
			},
		)

		if len(vertices) == 3 {
			totalVolume += signedTriangleVolume(
				vertices[0],
				vertices[1],
				vertices[2],
			)

			triangleCount++
			vertices = vertices[:0]
		}
	}

	if triangleCount == 0 {
		return 0, 0, fmt.Errorf(
			"no valid triangles found",
		)
	}

	return math.Abs(totalVolume), triangleCount, nil
}

func signedTriangleVolume(
	p1 Point,
	p2 Point,
	p3 Point,
) float64 {
	volume :=
		p1.X*(p2.Y*p3.Z-p2.Z*p3.Y) -
			p1.Y*(p2.X*p3.Z-p2.Z*p3.X) +
			p1.Z*(p2.X*p3.Y-p2.Y*p3.X)

	return volume / 6.0
}

func getMaterialDensity(
	material string,
) float64 {
	densities := map[string]float64{
		"PLA":   1.24,
		"PLA+":  1.24,
		"PETG":  1.27,
		"ABS":   1.04,
		"ASA":   1.07,
		"TPU":   1.21,
		"NYLON": 1.15,
	}

	if density, exists := densities[material]; exists {
		return density
	}

	// Default PLA density
	return 1.24
}

func estimatePrintWeight(
	solidWeight float64,
	infill float64,
) float64 {
	/*
		This is an estimate.

		Outer walls + top/bottom layers
		use material even when infill is low.
	*/

	estimatedMaterialFactor :=
		0.15 + (0.85 * infill / 100.0)

	return solidWeight *
		estimatedMaterialFactor
}

func round(
	value float64,
	places int,
) float64 {
	power := math.Pow10(places)

	return math.Round(value*power) / power
}

func sendError(
	w http.ResponseWriter,
	status int,
	message string,
) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	w.WriteHeader(status)

	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"success": false,
			"error":   message,
		},
	)
}
