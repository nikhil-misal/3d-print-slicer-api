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
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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
		"STL Analysis API running on port",
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
			"message": "STL Analysis API is running",
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
	volumeMM3, triangleCount, err :=
		calculateSTLVolume(fileData)

	if err != nil {
		sendError(
			w,
			http.StatusBadRequest,
			"Invalid or unsupported STL file: "+
				err.Error(),
		)
		return
	}

	// Convert mm³ → cm³
	volumeCm3 := volumeMM3 / 1000.0

	// Material density
	density := getMaterialDensity(
		material,
	)

	// Solid object weight
	solidWeight := volumeCm3 * density

	// Estimated printable weight
	estimatedWeight := estimatePrintWeight(
		solidWeight,
		infill,
	)

	response := AnalyzeResponse{
		Success: true,
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
		TriangleCount: triangleCount,
		Material: material,
		Infill: infill,
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
	Shopify Draft Order

	This endpoint creates a Draft Order using
	the calculated price sent by the Shopify
	frontend.

	The actual Shopify line-item price is set
	using priceOverride.
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

	/*
		Shopify Client Credentials token
	*/
	accessToken, err := getShopifyAccessToken()

	if err != nil {
		sendError(
			w,
			http.StatusInternalServerError,
			err.Error(),
		)
		return
	}

	shop := os.Getenv("SHOPIFY_SHOP")

	if shop == "" {
		sendError(
			w,
			http.StatusInternalServerError,
			"SHOPIFY_SHOP is missing",
		)
		return
	}

	/*
		Shopify Admin GraphQL endpoint
	*/
	graphqlURL :=
		"https://" +
			shop +
			".myshopify.com" +
			"/admin/api/2026-07/graphql.json"

	/*
		Custom attributes

		These are saved inside the Draft Order
		and will also be visible with the order.
	*/
	customAttributes := []map[string]string{
		{
			"key":   "Material",
			"value": request.Material,
		},
		{
			"key":   "Color",
			"value": request.Color,
		},
		{
			"key":   "Print Weight",
			"value": fmt.Sprintf(
				"%.2f g",
				request.Weight,
			),
		},
		{
			"key":   "Model Volume",
			"value": fmt.Sprintf(
				"%.2f cm³",
				request.Volume,
			),
		},
		{
			"key":   "STL File Name",
			"value": request.FileName,
		},
	}

	if request.FileURL != "" {
		customAttributes = append(
			customAttributes,
			map[string]string{
				"key":   "STL File URL",
				"value": request.FileURL,
			},
		)
	}

	if request.FileID != "" {
		customAttributes = append(
			customAttributes,
			map[string]string{
				"key":   "STL File ID",
				"value": request.FileID,
			},
		)
	}

	/*
		Shopify GraphQL mutation

		priceOverride is used so that the
		actual Draft Order price is the
		calculated custom-print price.
	*/
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

	/*
		Build variables.

		Price is per unit.
		Quantity is handled by Shopify.
	*/
	variables := map[string]interface{}{
		"input": map[string]interface{}{
			"lineItems": []interface{}{
				map[string]interface{}{
					"title": request.Title,
					"quantity": request.Quantity,
					"priceOverride": map[string]interface{}{
						"amount": fmt.Sprintf(
							"%.2f",
							request.Price,
						),
					},
					"customAttributes": customAttributes,
				},
			},
			"customAttributes": []map[string]string{
				{
					"key":   "Order Type",
					"value": "Custom 3D Print",
				},
			},
		},
	}

	payload := map[string]interface{}{
		"query":     query,
		"variables": variables,
	}

	payloadBytes, err := json.Marshal(
		payload,
	)

	if err != nil {
		sendError(
			w,
			http.StatusInternalServerError,
			"Could not create Shopify request",
		)
		return
	}

	req, err := http.NewRequest(
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

	resp, err := client.Do(req)

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

	responseBody, err := io.ReadAll(
		resp.Body,
	)

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

	/*
		Shopify response structure
	*/
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
					Field   []string `json:"field"`
					Message string   `json:"message"`
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

	/*
		GraphQL-level errors
	*/
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

	/*
		Draft Order validation errors
	*/
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

	/*
		Return checkout URL to frontend.
	*/
	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"success": true,
			"draftOrderId":
				draftOrderResult.DraftOrder.ID,
			"checkoutUrl":
				draftOrderResult.DraftOrder.InvoiceURL,
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

func calculateSTLVolume(
	data []byte,
) (float64, uint32, error) {
	if isBinarySTL(data) {
		return calculateBinarySTLVolume(data)
	}

	return calculateASCIISTLVolume(data)
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

	triangleCount :=
		binary.LittleEndian.Uint32(
			data[80:84],
		)

	expectedSize :=
		84 + int(triangleCount)*50

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

		totalVolume += signedTriangleVolume(
			p1,
			p2,
			p3,
		)
	}

	return math.Abs(
		totalVolume,
	), triangleCount, nil
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

func calculateASCIISTLVolume(
	data []byte,
) (float64, uint32, error) {
	lines := bytes.Split(
		data,
		[]byte("\n"),
	)

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

	return math.Abs(
		totalVolume,
	), triangleCount, nil
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
		"PETG": 1.27,
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
		0.15 +
			(0.85 * infill / 100.0)

	return solidWeight *
		estimatedMaterialFactor
}

func round(
	value float64,
	places int,
) float64 {
	power := math.Pow10(places)

	return math.Round(
		value*power,
	) / power
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
	<p>App is running successfully.</p>
</body>
</html>
`,
	)
}
```
