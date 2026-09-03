// Render deployment update
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Point struct {
	X float64
	Y float64
	Z float64
}

type Triangle struct {
	A int
	B int
	C int
}

type Mesh struct {
	Vertices  []Point
	Triangles []Triangle
}

type AnalyzeResponse struct {
	Success               bool    `json:"success"`
	FileType              string  `json:"file_type"`
	VolumeCm3             float64 `json:"volume_cm3"`
	SolidWeightG          float64 `json:"solid_weight_g"`
	EstimatedPrintWeightG float64 `json:"estimated_print_weight_g"`
	TriangleCount         uint32  `json:"triangle_count"`
	VertexCount           uint32  `json:"vertex_count"`
	Material              string  `json:"material"`
	Infill                float64 `json:"infill"`
	Unit                  string  `json:"unit"`
	VolumeMethod          string  `json:"volume_method"`
}

func getShopifyAccessToken() (string, error) {
	shop := os.Getenv("SHOPIFY_SHOP")
	clientID := os.Getenv("SHOPIFY_CLIENT_ID")
	clientSecret := os.Getenv("SHOPIFY_CLIENT_SECRET")

	if shop == "" || clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("Shopify environment variables are missing")
	}

	tokenURL := "https://" + shop + ".myshopify.com/admin/oauth/access_token"

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequest(
		http.MethodPost,
		tokenURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", err
	}

	req.Header.Set(
		"Content-Type",
		"application/x-www-form-urlencoded",
	)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"Shopify token request failed: %s",
			string(body),
		)
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	if result.AccessToken == "" {
		return "", fmt.Errorf(
			"Shopify access token was not returned",
		)
	}

	return result.AccessToken, nil
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", healthHandler)
	mux.HandleFunc("/app", appHandler)
	mux.HandleFunc("/analyze", analyzeHandler)
	mux.HandleFunc(
		"/create-draft-order",
		createDraftOrderHandler,
	)

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	fmt.Println(
		"3D Model Analysis API running on port",
		port,
	)

	err := http.ListenAndServe(
		":"+port,
		corsMiddleware(mux),
	)

	if err != nil {
		panic(err)
	}
}

func healthHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"success": true,
			"message": "3D Model Analysis API is running",
			"supported_formats": []string{
				"STL",
				"OBJ",
				"3MF",
				"PLY",
				"OFF",
				"AMF",
			},
		},
	)
}

func corsMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(
		func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			w.Header().Set(
				"Access-Control-Allow-Origin",
				"*",
			)

			w.Header().Set(
				"Access-Control-Allow-Methods",
				"POST, GET, OPTIONS",
			)

			w.Header().Set(
				"Access-Control-Allow-Headers",
				"Content-Type",
			)

			if r.Method == http.MethodOptions {
				w.WriteHeader(
					http.StatusNoContent,
				)
				return
			}

			next.ServeHTTP(w, r)
		},
	)
}

/*
	===========================================================
	MODEL ANALYSIS
	===========================================================
*/

func analyzeHandler(
	w http.ResponseWriter,
	r *http.Request,
) {
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

	err := r.ParseMultipartForm(
		50 << 20,
	)

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"File is too large or form is invalid",
		)
		return
	}

	file, header, err := r.FormFile("file")

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"3D model file is required",
		)
		return
	}

	defer file.Close()

	fileData, err := io.ReadAll(file)

	if err != nil {
		sendError(
			w,
			http.StatusInternalServerError,
			"Could not read 3D model file",
		)
		return
	}

	if len(fileData) == 0 {
		sendError(
			w,
			http.StatusBadRequest,
			"3D model file is empty",
		)
		return
	}

	/*
		Material
	*/
	material := strings.ToUpper(
		strings.TrimSpace(
			r.FormValue("material"),
		),
	)

	if material == "" {
		material = "PLA"
	}

	/*
		Infill
	*/
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

	/*
		Detect file type
	*/
	fileType := detectFileType(
		header.Filename,
		fileData,
	)

	if fileType == "" {
		sendError(
			w,
			http.StatusBadRequest,
			"Unsupported 3D file format. Supported formats: STL, OBJ, 3MF, PLY, OFF and AMF",
		)
		return
	}

	fmt.Println(
		"Analyzing:",
		header.Filename,
		"Format:",
		fileType,
		"Size:",
		len(fileData),
	)

	/*
		Parse mesh
	*/
	mesh, unit, err := parseModel(
		fileType,
		fileData,
	)

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"Could not read 3D model: "+err.Error(),
		)
		return
	}

	if len(mesh.Vertices) < 3 {
		sendError(
			w,
			http.StatusBadRequest,
			"3D model does not contain enough vertices",
		)
		return
	}

	if len(mesh.Triangles) == 0 {
		sendError(
			w,
			http.StatusBadRequest,
			"3D model does not contain any valid triangles",
		)
		return
	}

	/*
		Validate coordinates
	*/
	for _, p := range mesh.Vertices {
		if math.IsNaN(p.X) ||
			math.IsNaN(p.Y) ||
			math.IsNaN(p.Z) ||
			math.IsInf(p.X, 0) ||
			math.IsInf(p.Y, 0) ||
			math.IsInf(p.Z, 0) {

			sendError(
				w,
				http.StatusBadRequest,
				"3D model contains invalid coordinates",
			)
			return
		}
	}

	/*
		Convert source units to millimetres.
	*/
	unitScale := unitToMillimetres(unit)

	if unitScale <= 0 {
		unitScale = 1
		unit = "mm"
	}

	for i := range mesh.Vertices {
		mesh.Vertices[i].X *= unitScale
		mesh.Vertices[i].Y *= unitScale
		mesh.Vertices[i].Z *= unitScale
	}

	/*
		Calculate volume.
	*/
	volumeMM3, volumeMethod, err :=
		calculateMeshVolume(&mesh)

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"Could not calculate a reliable model volume: "+
				err.Error(),
		)
		return
	}

	if volumeMM3 <= 0 ||
		math.IsNaN(volumeMM3) ||
		math.IsInf(volumeMM3, 0) {

		sendError(
			w,
			http.StatusBadRequest,
			"Model volume could not be calculated reliably. Please check that the model is a closed solid mesh.",
		)
		return
	}

	/*
		mm³ -> cm³
	*/
	volumeCm3 := volumeMM3 / 1000.0

	if volumeCm3 <= 0 {
		sendError(
			w,
			http.StatusBadRequest,
			"Model volume is zero",
		)
		return
	}

	/*
		Material density
	*/
	density := getMaterialDensity(
		material,
	)

	/*
		Solid weight
	*/
	solidWeight := volumeCm3 * density

	/*
		Estimated printable weight
	*/
	estimatedWeight := estimatePrintWeight(
		solidWeight,
		infill,
	)

	if estimatedWeight <= 0 ||
		math.IsNaN(estimatedWeight) ||
		math.IsInf(estimatedWeight, 0) {

		sendError(
			w,
			http.StatusBadRequest,
			"Could not calculate model weight",
		)
		return
	}

	response := AnalyzeResponse{
		Success: true,
		FileType: fileType,

		VolumeCm3: round(
			volumeCm3,
			2,
		),

		SolidWeightG: round(
			solidWeight,
			2,
		),

		EstimatedPrintWeightG: round(
			estimatedWeight,
			2,
		),

		TriangleCount: uint32(
			len(mesh.Triangles),
		),

		VertexCount: uint32(
			len(mesh.Vertices),
		),

		Material: material,
		Infill:   infill,
		Unit:     "mm",

		VolumeMethod: volumeMethod,
	}

	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	json.NewEncoder(w).Encode(
		response,
	)
}

/*
	===========================================================
	FILE TYPE DETECTION
	===========================================================
*/

func detectFileType(
	filename string,
	data []byte,
) string {

	ext := strings.ToLower(
		filepath.Ext(filename),
	)

	switch ext {
	case ".stl":
		return "STL"

	case ".obj":
		return "OBJ"

	case ".3mf":
		return "3MF"

	case ".ply":
		return "PLY"

	case ".off":
		return "OFF"

	case ".amf":
		return "AMF"
	}

	/*
		Extension missing or incorrect.
		Try content detection.
	*/

	if isBinarySTL(data) ||
		looksLikeASCIISTL(data) {

		return "STL"
	}

	if bytes.HasPrefix(
		data,
		[]byte("OFF"),
	) {
		return "OFF"
	}

	if bytes.HasPrefix(
		data,
		[]byte("ply"),
	) {
		return "PLY"
	}

	if bytes.Contains(
		data,
		[]byte("<model"),
	) &&
		bytes.Contains(
			data,
			[]byte("<vertices"),
		) {

		return "AMF"
	}

	if bytes.Contains(
		data,
		[]byte("<model"),
	) &&
		bytes.Contains(
			data,
			[]byte("<resources"),
		) {

		return "3MF"
	}

	if looksLikeOBJ(data) {
		return "OBJ"
	}

	return ""
}

/*
	===========================================================
	STL
	===========================================================
*/

func calculateSTLMesh(
	data []byte,
) (Mesh, string, error) {

	if isBinarySTL(data) {
		mesh, err := parseBinarySTL(data)

		return mesh, "mm", err
	}

	return parseASCIISTL(data)
}

func isBinarySTL(
	data []byte,
) bool {
	if len(data) < 84 {
		return false
	}

	triangleCount :=
		binary.LittleEndian.Uint32(
			data[80:84],
		)

	expectedSize :=
		84 + int(triangleCount)*50

	return expectedSize == len(data)
}

func looksLikeASCIISTL(
	data []byte,
) bool {

	text := strings.ToLower(
		string(data[:minInt(len(data), 4096)]),
	)

	return strings.Contains(
		text,
		"facet",
	) &&
		strings.Contains(
			text,
			"vertex",
		)
}

func parseBinarySTL(
	data []byte,
) (Mesh, error) {

	if len(data) < 84 {
		return Mesh{}, fmt.Errorf(
			"binary STL file is too small",
		)
	}

	triangleCount :=
		binary.LittleEndian.Uint32(
			data[80:84],
		)

	expectedSize :=
		84 + int(triangleCount)*50

	if len(data) < expectedSize {
		return Mesh{}, fmt.Errorf(
			"binary STL file is incomplete",
		)
	}

	mesh := Mesh{
		Vertices:  make([]Point, 0, int(triangleCount)*3),
		Triangles: make([]Triangle, 0, int(triangleCount)),
	}

	offset := 84

	for i := uint32(0); i < triangleCount; i++ {

		// Skip normal
		offset += 12

		p1 := readBinaryPoint(
			data[offset:],
		)
		offset += 12

		p2 := readBinaryPoint(
			data[offset:],
		)
		offset += 12

		p3 := readBinaryPoint(
			data[offset:],
		)
		offset += 12

		// Skip attribute byte count
		offset += 2

		base := len(mesh.Vertices)

		mesh.Vertices = append(
			mesh.Vertices,
			p1,
			p2,
			p3,
		)

		mesh.Triangles = append(
			mesh.Triangles,
			Triangle{
				A: base,
				B: base + 1,
				C: base + 2,
			},
		)
	}

	return mesh, nil
}

func readBinaryPoint(
	data []byte,
) Point {

	return Point{
		X: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(
					data[0:4],
				),
			),
		),

		Y: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(
					data[4:8],
				),
			),
		),

		Z: float64(
			math.Float32frombits(
				binary.LittleEndian.Uint32(
					data[8:12],
				),
			),
		),
	}
}

func parseASCIISTL(
	data []byte,
) (Mesh, string, error) {

	scanner := bufio.NewScanner(
		bytes.NewReader(data),
	)

	/*
		Allow long STL lines.
	*/
	scanner.Buffer(
		make([]byte, 1024),
		1024*1024,
	)

	mesh := Mesh{
		Vertices:  make([]Point, 0),
		Triangles: make([]Triangle, 0),
	}

	triangleVertices :=
		make([]Point, 0, 3)

	for scanner.Scan() {

		line := strings.TrimSpace(
			scanner.Text(),
		)

		fields := strings.Fields(line)

		if len(fields) != 4 {
			continue
		}

		if strings.ToLower(fields[0]) != "vertex" {
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

		triangleVertices = append(
			triangleVertices,
			Point{
				X: x,
				Y: y,
				Z: z,
			},
		)

		if len(triangleVertices) == 3 {

			base := len(mesh.Vertices)

			mesh.Vertices = append(
				mesh.Vertices,
				triangleVertices[0],
				triangleVertices[1],
				triangleVertices[2],
			)

			mesh.Triangles = append(
				mesh.Triangles,
				Triangle{
					A: base,
					B: base + 1,
					C: base + 2,
				},
			)

			triangleVertices =
				triangleVertices[:0]
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, "mm", err
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"no valid STL triangles found",
		)
	}

	return mesh, "mm", nil
}

/*
	===========================================================
	OBJ
	===========================================================
*/

func looksLikeOBJ(
	data []byte,
) bool {

	text := string(
		data[:minInt(len(data), 8192)],
	)

	lines := strings.Split(
		text,
		"\n",
	)

	for _, line := range lines {

		line = strings.TrimSpace(line)

		if strings.HasPrefix(
			line,
			"v ",
		) ||
			strings.HasPrefix(
				line,
				"f ",
			) {

			return true
		}
	}

	return false
}

func parseOBJ(
	data []byte,
) (Mesh, string, error) {

	scanner := bufio.NewScanner(
		bytes.NewReader(data),
	)

	scanner.Buffer(
		make([]byte, 1024),
		1024*1024,
	)

	mesh := Mesh{
		Vertices:  make([]Point, 0),
		Triangles: make([]Triangle, 0),
	}

	for scanner.Scan() {

		line := strings.TrimSpace(
			scanner.Text(),
		)

		if line == "" ||
			strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)

		if len(fields) == 0 {
			continue
		}

		switch fields[0] {

		case "v":

			if len(fields) < 4 {
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

			mesh.Vertices = append(
				mesh.Vertices,
				Point{
					X: x,
					Y: y,
					Z: z,
				},
			)

		case "f":

			if len(fields) < 4 {
				continue
			}

			indices :=
				make([]int, 0, len(fields)-1)

			for _, value := range fields[1:] {

				indexString :=
					strings.Split(
						value,
						"/",
					)[0]

				index, err :=
					strconv.Atoi(
						indexString,
					)

				if err != nil {
					continue
				}

				/*
					OBJ supports negative indices.
				*/
				if index < 0 {
					index =
						len(mesh.Vertices) +
							index +
							1
				}

				index--

				if index < 0 ||
					index >= len(mesh.Vertices) {
					continue
				}

				indices = append(
					indices,
					index,
				)
			}

			/*
				Triangulate polygon using fan.
			*/
			for i := 1; i+1 < len(indices); i++ {

				mesh.Triangles = append(
					mesh.Triangles,
					Triangle{
						A: indices[0],
						B: indices[i],
						C: indices[i+1],
					},
				)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, "mm", err
	}

	if len(mesh.Vertices) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"OBJ contains no vertices",
		)
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"OBJ contains no valid faces",
		)
	}

	/*
		OBJ has no universal unit declaration.
		For 3D printing we treat it as millimetres.
	*/
	return mesh, "mm", nil
}

/*
	===========================================================
	OFF
	===========================================================
*/

func parseOFF(
	data []byte,
) (Mesh, string, error) {

	scanner := bufio.NewScanner(
		bytes.NewReader(data),
	)

	scanner.Buffer(
		make([]byte, 1024),
		1024*1024,
	)

	tokens := make([]string, 0)

	for scanner.Scan() {

		line := strings.TrimSpace(
			scanner.Text(),
		)

		if line == "" ||
			strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)

		for _, part := range parts {
			tokens = append(tokens, part)
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, "mm", err
	}

	if len(tokens) < 4 {
		return Mesh{}, "mm", fmt.Errorf(
			"OFF file is incomplete",
		)
	}

	position := 0

	if strings.ToUpper(tokens[0]) == "OFF" {
		position++
	}

	if position+2 >= len(tokens) {
		return Mesh{}, "mm", fmt.Errorf(
			"OFF header is invalid",
		)
	}

	vertexCount, err1 :=
		strconv.Atoi(tokens[position])

	faceCount, err2 :=
		strconv.Atoi(tokens[position+1])

	if err1 != nil || err2 != nil {
		return Mesh{}, "mm", fmt.Errorf(
			"OFF counts are invalid",
		)
	}

	position += 3

	mesh := Mesh{
		Vertices:  make([]Point, 0, vertexCount),
		Triangles: make([]Triangle, 0),
	}

	for i := 0; i < vertexCount; i++ {

		if position+2 >= len(tokens) {
			return Mesh{}, "mm", fmt.Errorf(
				"OFF vertex data is incomplete",
			)
		}

		x, e1 := strconv.ParseFloat(
			tokens[position],
			64,
		)

		y, e2 := strconv.ParseFloat(
			tokens[position+1],
			64,
		)

		z, e3 := strconv.ParseFloat(
			tokens[position+2],
			64,
		)

		if e1 != nil ||
			e2 != nil ||
			e3 != nil {
			return Mesh{}, "mm", fmt.Errorf(
				"invalid OFF vertex",
			)
		}

		mesh.Vertices = append(
			mesh.Vertices,
			Point{
				X: x,
				Y: y,
				Z: z,
			},
		)

		position += 3
	}

	for i := 0; i < faceCount; i++ {

		if position >= len(tokens) {
			break
		}

		n, err :=
			strconv.Atoi(tokens[position])

		if err != nil {
			return Mesh{}, "mm", fmt.Errorf(
				"invalid OFF face",
			)
		}

		position++

		indices := make([]int, 0, n)

		for j := 0; j < n; j++ {

			if position >= len(tokens) {
				return Mesh{}, "mm", fmt.Errorf(
					"OFF face is incomplete",
				)
			}

			index, err :=
				strconv.Atoi(tokens[position])

			if err != nil {
				return Mesh{}, "mm", fmt.Errorf(
					"invalid OFF face index",
				)
			}

			indices = append(
				indices,
				index,
			)

			position++
		}

		for j := 1; j+1 < len(indices); j++ {

			mesh.Triangles = append(
				mesh.Triangles,
				Triangle{
					A: indices[0],
					B: indices[j],
					C: indices[j+1],
				},
			)
		}
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"OFF contains no valid triangles",
		)
	}

	return mesh, "mm", nil
}

/*
	===========================================================
	PLY
	===========================================================
*/

func parsePLY(
	data []byte,
) (Mesh, string, error) {

	/*
		For safety and predictable parsing,
		this implementation supports ASCII PLY.
	*/
	headerEnd :=
		bytes.Index(
			data,
			[]byte("end_header"),
		)

	if headerEnd < 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"PLY header is missing",
		)
	}

	headerEnd += len("end_header")

	headerText :=
		string(data[:headerEnd])

	lowerHeader :=
		strings.ToLower(headerText)

	if !strings.Contains(
		lowerHeader,
		"format ascii",
	) {

		return Mesh{}, "mm", fmt.Errorf(
			"binary PLY is not supported yet; please upload ASCII PLY, STL, OBJ or 3MF",
		)
	}

	vertexCount := 0
	faceCount := 0

	for _, line :=
		range strings.Split(
			headerText,
			"\n",
		) {

		fields := strings.Fields(
			strings.TrimSpace(line),
		)

		if len(fields) >= 2 &&
			fields[0] == "element" {

			if fields[1] == "vertex" &&
				len(fields) >= 3 {

				vertexCount, _ =
					strconv.Atoi(fields[2])
			}

			if fields[1] == "face" &&
				len(fields) >= 3 {

				faceCount, _ =
					strconv.Atoi(fields[2])
			}
		}
	}

	if vertexCount <= 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"PLY contains no vertices",
		)
	}

	/*
		Move after header.
	*/
	bodyStart := headerEnd

	for bodyStart < len(data) &&
		(data[bodyStart] == '\n' ||
			data[bodyStart] == '\r') {

		bodyStart++
	}

	scanner := bufio.NewScanner(
		bytes.NewReader(
			data[bodyStart:],
		),
	)

	scanner.Buffer(
		make([]byte, 1024),
		1024*1024,
	)

	mesh := Mesh{
		Vertices: make(
			[]Point,
			0,
			vertexCount,
		),

		Triangles: make(
			[]Triangle,
			0,
		),
	}

	/*
		Read vertices.
	*/
	for len(mesh.Vertices) < vertexCount &&
		scanner.Scan() {

		fields := strings.Fields(
			scanner.Text(),
		)

		if len(fields) < 3 {
			continue
		}

		x, e1 := strconv.ParseFloat(
			fields[0],
			64,
		)

		y, e2 := strconv.ParseFloat(
			fields[1],
			64,
		)

		z, e3 := strconv.ParseFloat(
			fields[2],
			64,
		)

		if e1 != nil ||
			e2 != nil ||
			e3 != nil {

			return Mesh{}, "mm", fmt.Errorf(
				"invalid PLY vertex",
			)
		}

		mesh.Vertices = append(
			mesh.Vertices,
			Point{
				X: x,
				Y: y,
				Z: z,
			},
		)
	}

	if len(mesh.Vertices) != vertexCount {
		return Mesh{}, "mm", fmt.Errorf(
			"PLY vertex data is incomplete",
		)
	}

	/*
		Read faces.
	*/
	facesRead := 0

	for facesRead < faceCount &&
		scanner.Scan() {

		fields := strings.Fields(
			scanner.Text(),
		)

		if len(fields) < 4 {
			continue
		}

		n, err :=
			strconv.Atoi(fields[0])

		if err != nil ||
			n < 3 ||
			len(fields) < n+1 {

			continue
		}

		indices := make(
			[]int,
			0,
			n,
		)

		for i := 0; i < n; i++ {

			index, err :=
				strconv.Atoi(
					fields[i+1],
				)

			if err != nil ||
				index < 0 ||
				index >= len(mesh.Vertices) {

				continue
			}

			indices = append(
				indices,
				index,
			)
		}

		if len(indices) >= 3 {

			for i := 1; i+1 < len(indices); i++ {

				mesh.Triangles = append(
					mesh.Triangles,
					Triangle{
						A: indices[0],
						B: indices[i],
						C: indices[i+1],
					},
				)
			}
		}

		facesRead++
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"PLY contains no valid faces",
		)
	}

	return mesh, "mm", nil
}

/*
	===========================================================
	3MF
	===========================================================
*/

type threeMFModel struct {
	XMLName xml.Name `xml:"model"`

	Unit string `xml:"unit,attr"`

	Resources struct {
		Objects []threeMFObject `xml:"object"`
	} `xml:"resources"`
}

type threeMFObject struct {
	ID   int `xml:"id,attr"`
	Type string `xml:"type,attr"`

	Mesh struct {
		Vertices struct {
			Vertices []threeMFVertex `xml:"vertex"`
		} `xml:"vertices"`

		Triangles struct {
			Triangles []threeMFTriangle `xml:"triangle"`
		} `xml:"triangles"`
	} `xml:"mesh"`
}

type threeMFVertex struct {
	X float64 `xml:"x,attr"`
	Y float64 `xml:"y,attr"`
	Z float64 `xml:"z,attr"`
}

type threeMFTriangle struct {
	V1 int `xml:"v1,attr"`
	V2 int `xml:"v2,attr"`
	V3 int `xml:"v3,attr"`
}

func parse3MF(
	data []byte,
) (Mesh, string, error) {

	reader, err :=
		zip.NewReader(
			bytes.NewReader(data),
			int64(len(data)),
		)

	if err != nil {
		return Mesh{}, "mm", fmt.Errorf(
			"invalid 3MF ZIP container",
		)
	}

	var modelData []byte

	for _, file := range reader.File {

		name := strings.ToLower(
			file.Name,
		)

		if strings.HasSuffix(
			name,
			".model",
		) {

			rc, err := file.Open()

			if err != nil {
				continue
			}

			modelData, err =
				io.ReadAll(rc)

			rc.Close()

			if err == nil {
				break
			}
		}
	}

	if len(modelData) == 0 {
		return Mesh{}, "mm", fmt.Errorf(
			"3MF model file was not found",
		)
	}

	var model threeMFModel

	if err := xml.Unmarshal(
		modelData,
		&model,
	); err != nil {

		return Mesh{}, "mm", fmt.Errorf(
			"invalid 3MF model XML: %v",
			err,
		)
	}

	mesh := Mesh{
		Vertices:  make([]Point, 0),
		Triangles: make([]Triangle, 0),
	}

	for _, object := range model.Resources.Objects {

		if len(object.Mesh.Vertices.Vertices) == 0 {
			continue
		}

		base := len(mesh.Vertices)

		for _, vertex :=
			range object.Mesh.Vertices.Vertices {

			mesh.Vertices = append(
				mesh.Vertices,
				Point{
					X: vertex.X,
					Y: vertex.Y,
					Z: vertex.Z,
				},
			)
		}

		for _, triangle :=
			range object.Mesh.Triangles.Triangles {

			a := base + triangle.V1
			b := base + triangle.V2
			c := base + triangle.V3

			if a < base ||
				b < base ||
				c < base ||
				a >= len(mesh.Vertices) ||
				b >= len(mesh.Vertices) ||
				c >= len(mesh.Vertices) {

				continue
			}

			mesh.Triangles = append(
				mesh.Triangles,
				Triangle{
					A: a,
					B: b,
					C: c,
				},
			)
		}
	}

	unit := strings.ToLower(
		strings.TrimSpace(
			model.Unit,
		),
	)

	if unit == "" {
		unit = "millimeter"
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, unit, fmt.Errorf(
			"3MF contains no valid mesh triangles",
		)
	}

	return mesh, unit, nil
}

/*
	===========================================================
	AMF
	===========================================================
*/

type amfDocument struct {
	XMLName xml.Name `xml:"amf"`

	Unit string `xml:"unit,attr"`

	Objects []amfObject `xml:"object"`
}

type amfObject struct {
	Mesh amfMesh `xml:"mesh"`
}

type amfMesh struct {
	Vertices amfVertices `xml:"vertices"`

	Volumes []amfVolume `xml:"volume"`
}

type amfVertices struct {
	Vertices []amfVertex `xml:"vertex"`
}

type amfVertex struct {
	Coordinates amfCoordinates `xml:"coordinates"`
}

type amfCoordinates struct {
	X float64 `xml:"x"`
	Y float64 `xml:"y"`
	Z float64 `xml:"z"`
}

type amfVolume struct {
	Triangles []amfTriangle `xml:"triangle"`
}

type amfTriangle struct {
	V1 int `xml:"v1"`
	V2 int `xml:"v2"`
	V3 int `xml:"v3"`
}

func parseAMF(
	data []byte,
) (Mesh, string, error) {

	var document amfDocument

	if err := xml.Unmarshal(
		data,
		&document,
	); err != nil {

		return Mesh{}, "mm", fmt.Errorf(
			"invalid AMF XML: %v",
			err,
		)
	}

	mesh := Mesh{
		Vertices:  make([]Point, 0),
		Triangles: make([]Triangle, 0),
	}

	for _, object := range document.Objects {

		base := len(mesh.Vertices)

		for _, vertex :=
			range object.Mesh.Vertices.Vertices {

			mesh.Vertices = append(
				mesh.Vertices,
				Point{
					X: vertex.Coordinates.X,
					Y: vertex.Coordinates.Y,
					Z: vertex.Coordinates.Z,
				},
			)
		}

		for _, volume :=
			range object.Mesh.Volumes {

			for _, triangle :=
				range volume.Triangles {

				a := base + triangle.V1
				b := base + triangle.V2
				c := base + triangle.V3

				if a < base ||
					b < base ||
					c < base ||
					a >= len(mesh.Vertices) ||
					b >= len(mesh.Vertices) ||
					c >= len(mesh.Vertices) {

					continue
				}

				mesh.Triangles = append(
					mesh.Triangles,
					Triangle{
						A: a,
						B: b,
						C: c,
					},
				)
			}
		}
	}

	unit := strings.ToLower(
		strings.TrimSpace(
			document.Unit,
		),
	)

	if unit == "" {
		unit = "millimeter"
	}

	if len(mesh.Triangles) == 0 {
		return Mesh{}, unit, fmt.Errorf(
			"AMF contains no valid triangles",
		)
	}

	return mesh, unit, nil
}

/*
	===========================================================
	GENERIC MODEL PARSER
	===========================================================
*/

func parseModel(
	fileType string,
	data []byte,
) (Mesh, string, error) {

	switch fileType {

	case "STL":
		return calculateSTLMesh(data)

	case "OBJ":
		return parseOBJ(data)

	case "PLY":
		return parsePLY(data)

	case "OFF":
		return parseOFF(data)

	case "3MF":
		return parse3MF(data)

	case "AMF":
		return parseAMF(data)

	default:
		return Mesh{}, "mm", fmt.Errorf(
			"unsupported model format: %s",
			fileType,
		)
	}
}

/*
	===========================================================
	ROBUST VOLUME CALCULATION
	===========================================================
*/

func calculateMeshVolume(
	mesh *Mesh,
) (float64, string, error) {

	if mesh == nil {
		return 0, "", fmt.Errorf(
			"mesh is nil",
		)
	}

	if len(mesh.Vertices) == 0 ||
		len(mesh.Triangles) == 0 {

		return 0, "", fmt.Errorf(
			"mesh is empty",
		)
	}

	/*
		First method:
		standard signed tetrahedron volume.

		This is exact for a closed mesh with
		consistent triangle orientation.
	*/
	signedVolume :=
		calculateSignedMeshVolume(
			mesh,
		)

	absSigned := math.Abs(
		signedVolume,
	)

	/*
		Find a safe scale to determine whether
		the signed result has suffered cancellation.
	*/
	bboxDiagonal :=
		meshBoundingBoxDiagonal(mesh)

	scaleVolume :=
		bboxDiagonal *
			bboxDiagonal *
			bboxDiagonal

	if scaleVolume <= 0 {
		scaleVolume = 1
	}

	/*
		If signed volume is healthy, use it.
	*/
	if absSigned > scaleVolume*1e-12 {

		return absSigned,
			"watertight_signed",
			nil
	}

	/*
		Fallback:
		Use absolute tetrahedral contributions
		around the mesh bounding-box center.

		This is specifically useful for STL files
		where triangle winding is inconsistent.

		For a normal closed printable mesh this
		recovers volume even when face directions
		are mixed.
	*/
	fallbackVolume :=
		calculateAbsoluteCenteredVolume(
			mesh,
		)

	if fallbackVolume > 0 &&
		!math.IsNaN(fallbackVolume) &&
		!math.IsInf(fallbackVolume, 0) {

		return fallbackVolume,
			"orientation_fallback",
			nil
	}

	return 0, "", fmt.Errorf(
		"mesh appears to be open, empty or invalid",
	)
}

func calculateSignedMeshVolume(
	mesh *Mesh,
) float64 {

	total := 0.0

	for _, triangle :=
		range mesh.Triangles {

		if triangle.A < 0 ||
			triangle.B < 0 ||
			triangle.C < 0 ||
			triangle.A >= len(mesh.Vertices) ||
			triangle.B >= len(mesh.Vertices) ||
			triangle.C >= len(mesh.Vertices) {

			continue
		}

		p1 :=
			mesh.Vertices[triangle.A]

		p2 :=
			mesh.Vertices[triangle.B]

		p3 :=
			mesh.Vertices[triangle.C]

		total +=
			signedTriangleVolume(
				p1,
				p2,
				p3,
			)
	}

	/*
		Input coordinates are in mm.
		Result is mm³.
	*/
	return total
}

func calculateAbsoluteCenteredVolume(
	mesh *Mesh,
) float64 {

	center :=
		meshBoundingBoxCenter(mesh)

	total := 0.0

	for _, triangle :=
		range mesh.Triangles {

		if triangle.A < 0 ||
			triangle.B < 0 ||
			triangle.C < 0 ||
			triangle.A >= len(mesh.Vertices) ||
			triangle.B >= len(mesh.Vertices) ||
			triangle.C >= len(mesh.Vertices) {

			continue
		}

		p1 := subtractPoint(
			mesh.Vertices[triangle.A],
			center,
		)

		p2 := subtractPoint(
			mesh.Vertices[triangle.B],
			center,
		)

		p3 := subtractPoint(
			mesh.Vertices[triangle.C],
			center,
		)

		total += math.Abs(
			signedTriangleVolume(
				p1,
				p2,
				p3,
			),
		)
	}

	return total
}

func meshBoundingBoxCenter(
	mesh *Mesh,
) Point {

	if len(mesh.Vertices) == 0 {
		return Point{}
	}

	minX := mesh.Vertices[0].X
	maxX := mesh.Vertices[0].X

	minY := mesh.Vertices[0].Y
	maxY := mesh.Vertices[0].Y

	minZ := mesh.Vertices[0].Z
	maxZ := mesh.Vertices[0].Z

	for _, p :=
		range mesh.Vertices {

		if p.X < minX {
			minX = p.X
		}

		if p.X > maxX {
			maxX = p.X
		}

		if p.Y < minY {
			minY = p.Y
		}

		if p.Y > maxY {
			maxY = p.Y
		}

		if p.Z < minZ {
			minZ = p.Z
		}

		if p.Z > maxZ {
			maxZ = p.Z
		}
	}

	return Point{
		X: (minX + maxX) / 2,
		Y: (minY + maxY) / 2,
		Z: (minZ + maxZ) / 2,
	}
}

func meshBoundingBoxDiagonal(
	mesh *Mesh,
) float64 {

	if len(mesh.Vertices) == 0 {
		return 0
	}

	minX := mesh.Vertices[0].X
	maxX := mesh.Vertices[0].X

	minY := mesh.Vertices[0].Y
	maxY := mesh.Vertices[0].Y

	minZ := mesh.Vertices[0].Z
	maxZ := mesh.Vertices[0].Z

	for _, p :=
		range mesh.Vertices {

		minX = math.Min(minX, p.X)
		maxX = math.Max(maxX, p.X)

		minY = math.Min(minY, p.Y)
		maxY = math.Max(maxY, p.Y)

		minZ = math.Min(minZ, p.Z)
		maxZ = math.Max(maxZ, p.Z)
	}

	dx := maxX - minX
	dy := maxY - minY
	dz := maxZ - minZ

	return math.Sqrt(
		dx*dx +
			dy*dy +
			dz*dz,
	)
}

func subtractPoint(
	a Point,
	b Point,
) Point {

	return Point{
		X: a.X - b.X,
		Y: a.Y - b.Y,
		Z: a.Z - b.Z,
	}
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

/*
	===========================================================
	UNIT CONVERSION
	===========================================================
*/

func unitToMillimetres(
	unit string,
) float64 {

	switch strings.ToLower(
		strings.TrimSpace(unit),
	) {

	case "mm":
		return 1

	case "millimeter":
		return 1

	case "millimetre":
		return 1

	case "cm":
		return 10

	case "centimeter":
		return 10

	case "centimetre":
		return 10

	case "m":
		return 1000

	case "meter":
		return 1000

	case "metre":
		return 1000

	case "in":
		return 25.4

	case "inch":
		return 25.4

	case "ft":
		return 304.8

	case "foot":
		return 304.8

	case "micron":
		return 0.001

	case "micrometer":
		return 0.001

	default:
		return 1
	}
}

/*
	===========================================================
	MATERIAL / WEIGHT
	===========================================================
*/

func getMaterialDensity(
	material string,
) float64 {

	densities := map[string]float64{

		"PLA": 1.24,

		"PLA+": 1.24,

		"PETG": 1.27,

		"ABS": 1.04,

		"ASA": 1.07,

		"TPU": 1.21,

		"NYLON": 1.15,
	}

	if density, exists :=
		densities[material]; exists {

		return density
	}

	/*
		Default PLA density.
	*/
	return 1.24
}

func estimatePrintWeight(
	solidWeight float64,
	infill float64,
) float64 {

	/*
		This remains an estimate.

		15% base material represents:
		- outer walls
		- top/bottom layers
		- structural material

		The remaining 85% follows the
		selected infill percentage.
	*/

	estimatedMaterialFactor :=
		0.15 +
			(0.85 * infill / 100.0)

	return solidWeight *
		estimatedMaterialFactor
}

/*
	===========================================================
	SHOPIFY DRAFT ORDER
	===========================================================
*/

type DraftOrderRequest struct {
	Price           float64 `json:"price"`
	Quantity        int     `json:"quantity"`
	Title           string  `json:"title"`
	Material        string  `json:"material"`
	Color           string  `json:"color"`
	Weight          float64 `json:"weight"`
	Volume          float64 `json:"volume"`
	FileName        string  `json:"fileName"`
	FileURL         string  `json:"fileUrl"`
	FileID          string  `json:"fileId"`
}

func createDraftOrderHandler(
	w http.ResponseWriter,
	r *http.Request,
) {

	if r.Method != http.MethodPost {

		sendError(
			w,
			http.StatusMethodNotAllowed,
			"Only POST requests are allowed",
		)

		return
	}

	w.Header().Set(
		"Content-Type",
		"application/json",
	)

	var request DraftOrderRequest

	err := json.NewDecoder(
		r.Body,
	).Decode(&request)

	if err != nil {

		sendError(
			w,
			http.StatusBadRequest,
			"Invalid JSON request",
		)

		return
	}

	if request.Price <= 0 {

		sendError(
			w,
			http.StatusBadRequest,
			"Price must be greater than zero",
		)

		return
	}

	if request.Quantity < 1 {
		request.Quantity = 1
	}

	if request.Title == "" {
		request.Title = "Custom 3D Print"
	}

	accessToken, err :=
		getShopifyAccessToken()

	if err != nil {

		sendError(
			w,
			http.StatusInternalServerError,
			err.Error(),
		)

		return
	}

	shop :=
		os.Getenv("SHOPIFY_SHOP")

	if shop == "" {

		sendError(
			w,
			http.StatusInternalServerError,
			"SHOPIFY_SHOP is missing",
		)

		return
	}

	graphqlURL :=
		"https://" +
			shop +
			".myshopify.com" +
			"/admin/api/2026-07/graphql.json"

	customAttributes :=
		[]map[string]string{

			{
				"key": "Material",
				"value": request.Material,
			},

			{
				"key": "Color",
				"value": request.Color,
			},

			{
				"key": "Print Weight",
				"value": fmt.Sprintf(
					"%.2f g",
					request.Weight,
				),
			},

			{
				"key": "Model Volume",
				"value": fmt.Sprintf(
					"%.2f cm³",
					request.Volume,
				),
			},

			{
				"key": "STL File Name",
				"value": request.FileName,
			},
		}

	if request.FileURL != "" {

		customAttributes =
			append(
				customAttributes,
				map[string]string{
					"key": "STL File URL",
					"value": request.FileURL,
				},
			)
	}

	if request.FileID != "" {

		customAttributes =
			append(
				customAttributes,
				map[string]string{
					"key": "STL File ID",
					"value": request.FileID,
				},
			)
	}

	query := `
mutation draftOrderCreate(
	$input: DraftOrderInput!
) {
	draftOrderCreate(input: $input) {
		draftOrder {
			id
			invoiceUrl
			totalPriceSet {
				shopMoney {
					amount
					currencyCode
				}
			}
		}
		userErrors {
			field
			message
		}
	}
}
`

	variables :=
		map[string]interface{}{

			"input":
				map[string]interface{}{

					"lineItems":
						[]interface{}{

							map[string]interface{}{

								"title":
									request.Title,

								"quantity":
									request.Quantity,

								"originalUnitPriceWithCurrency":
									map[string]interface{}{

										"amount":
											fmt.Sprintf(
												"%.2f",
												request.Price,
											),

										"currencyCode":
											"INR",
									},

								"customAttributes":
									customAttributes,
							},
						},

					"customAttributes":
						[]map[string]string{

							{
								"key":
									"Order Type",

								"value":
									"Custom 3D Print",
							},
						},
				},
		}

	payload :=
		map[string]interface{}{
			"query":     query,
			"variables": variables,
		}

	payloadBytes, err :=
		json.Marshal(payload)

	if err != nil {

		sendError(
			w,
			http.StatusInternalServerError,
			"Could not create Shopify request",
		)

		return
	}

	req, err :=
		http.NewRequest(
			http.MethodPost,
			graphqlURL,
			bytes.NewReader(payloadBytes),
		)

	if err != nil {

		sendError(
			w,
			http.StatusInternalServerError,
			"Could not create Shopify request",
		)

		return
	}

	req.Header.Set(
		"Content-Type",
		"application/json",
	)

	req.Header.Set(
		"X-Shopify-Access-Token",
		accessToken,
	)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err :=
		client.Do(req)

	if err != nil {

		sendError(
			w,
			http.StatusBadGateway,
			"Could not connect to Shopify: "+
				err.Error(),
		)

		return
	}

	defer resp.Body.Close()

	responseBody, err :=
		io.ReadAll(resp.Body)

	if err != nil {

		sendError(
			w,
			http.StatusBadGateway,
			"Could not read Shopify response",
		)

		return
	}

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		sendError(
			w,
			http.StatusBadGateway,
			"Shopify API returned an error: "+
				string(responseBody),
		)

		return
	}

	var shopifyResponse struct {

		Data struct {

			DraftOrderCreate struct {

				DraftOrder *struct {

					ID string `json:"id"`

					InvoiceURL string `json:"invoiceUrl"`

					TotalPriceSet struct {

						ShopMoney struct {

							Amount string `json:"amount"`

							CurrencyCode string `json:"currencyCode"`

						} `json:"shopMoney"`

					} `json:"totalPriceSet"`

				} `json:"draftOrder"`

				UserErrors []struct {

					Field []string `json:"field"`

					Message string `json:"message"`

				} `json:"userErrors"`

			} `json:"draftOrderCreate"`

		} `json:"data"`

		Errors []struct {

			Message string `json:"message"`

		} `json:"errors"`
	}

	err = json.Unmarshal(
		responseBody,
		&shopifyResponse,
	)

	if err != nil {

		sendError(
			w,
			http.StatusBadGateway,
			"Invalid Shopify response: "+
				err.Error(),
		)

		return
	}

	if len(shopifyResponse.Errors) > 0 {

		sendError(
			w,
			http.StatusBadGateway,
			"Shopify GraphQL error: "+
				shopifyResponse.Errors[0].Message,
		)

		return
	}

	draftOrderResult :=
		shopifyResponse.
			Data.
			DraftOrderCreate

	if len(draftOrderResult.UserErrors) > 0 {

		sendError(
			w,
			http.StatusBadRequest,
			draftOrderResult.UserErrors[0].Message,
		)

		return
	}

	if draftOrderResult.DraftOrder == nil {

		sendError(
			w,
			http.StatusBadGateway,
			"Shopify did not create the Draft Order",
		)

		return
	}

	json.NewEncoder(w).Encode(
		map[string]interface{}{

			"success": true,

			"draftOrderId":
				draftOrderResult.
					DraftOrder.
					ID,

			"checkoutUrl":
				draftOrderResult.
					DraftOrder.
					InvoiceURL,

			"totalPrice":
				draftOrderResult.
					DraftOrder.
					TotalPriceSet.
					ShopMoney.
					Amount,

			"currency":
				draftOrderResult.
					DraftOrder.
					TotalPriceSet.
					ShopMoney.
					CurrencyCode,
		},
	)
}

/*
	===========================================================
	HELPERS
	===========================================================
*/

func round(
	value float64,
	places int,
) float64 {

	power := math.Pow10(
		places,
	)

	return math.Round(
		value*power,
	) / power
}

func minInt(
	a int,
	b int,
) int {

	if a < b {
		return a
	}

	return b
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

/*
	===========================================================
	APP
	===========================================================
*/

func appHandler(
	w http.ResponseWriter,
	r *http.Request,
) {

	w.Header().Set(
		"Content-Type",
		"text/html; charset=utf-8",
	)

	fmt.Fprint(
		w,
		`
<!DOCTYPE html>
<html>
<head>
	<title>Custom 3D Print Orders</title>
</head>
<body>
	<h1>Custom 3D Print Orders</h1>

	<p>3D Model Analysis API is running successfully.</p>

	<p>
	Supported formats:
	<strong>
	STL, OBJ, 3MF, PLY, OFF, AMF
	</strong>
	</p>

</body>
</html>
`,
	)
}
