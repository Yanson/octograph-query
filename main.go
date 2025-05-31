package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"image/color"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"text/template"

	//"os"
	"sort"
	"time"

	"github.com/fogleman/gg"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type datapoint struct {
	Time  time.Time
	Value float64
}

var (
	influxURL    = os.Getenv("INFLUXDB_URL")
	influxToken  = os.Getenv("INFLUXDB_TOKEN")
	influxOrg    = os.Getenv("INFLUXDB_ORG")
	influxBucket = os.Getenv("INFLUXDB_BUCKET")
)

var (
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests processed, labeled by status code and method.",
		},
		[]string{"code", "method"},
	)
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Duration of HTTP requests.",
		},
		[]string{"handler", "method"},
	)
	queryCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "octograph_query_requests_total",
			Help: "Total number of /query requests.",
		},
		[]string{"format"},
	)
)

func init() {
	prometheus.MustRegister(httpRequests, httpDuration, queryCounter)
}

func main() {
	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}

	http.Handle("/monitoring/metrics", promhttp.Handler())

	http.HandleFunc("/monitoring/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	http.Handle("/query", promhttp.InstrumentHandlerDuration(
		httpDuration.MustCurryWith(prometheus.Labels{"handler": "query"}),
		promhttp.InstrumentHandlerCounter(httpRequests, http.HandlerFunc(handleQuery)),
	))

	log.Println("Starting server on port", httpPort)
	log.Fatal(http.ListenAndServe(":"+httpPort, nil))
}

type QueryParams struct {
	Location string
	Format   string
	Width    int
	Height   int
	DebugNow time.Time
	Template string
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	params, err := parseQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		log.Println("Failed to load UK timezone:", err)
		loc = time.UTC // fallback
	}

	var now time.Time
	if !params.DebugNow.IsZero() {
		now = params.DebugNow.In(loc)
	} else {
		now = time.Now().In(loc)
	}
	log.Printf("Assuming it is now: %s", now.Format(time.RFC3339))

	points, err := queryDatapoints(params, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	addExpiryHeader(w)

	format := params.Format
	switch params.Format {
	case "html":
		if err := renderHtml(w, params); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case "png":
		renderPng(w, points, params.Width, params.Height, now)
	case "csv":
		renderCsv(w, points)
	default:
		format = "invalid"
		http.Error(w, fmt.Errorf("Invalid 'format' specified").Error(), http.StatusBadRequest)
	}
	queryCounter.WithLabelValues(format).Inc()
}

func parseQueryParams(r *http.Request) (QueryParams, error) {
	location := r.URL.Query().Get("location")
	if location == "" {
		return QueryParams{}, fmt.Errorf("missing 'location' query parameter")
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "html"
	}

	width := 1014
	height := 474

	if wStr := r.URL.Query().Get("width"); wStr != "" {
		wVal, err := strconv.Atoi(wStr)
		if err != nil || wVal < 200 || wVal > 2000 {
			return QueryParams{}, fmt.Errorf("invalid 'width' query parameter")
		}
		width = wVal
	}

	if hStr := r.URL.Query().Get("height"); hStr != "" {
		hVal, err := strconv.Atoi(hStr)
		if err != nil || hVal < 100 || hVal > 1200 {
			return QueryParams{}, fmt.Errorf("invalid 'height' query parameter")
		}
		height = hVal
	}

	// Parse optional debugNow parameter
	var debugNow time.Time
	if debugTimeStr := r.URL.Query().Get("debugNow"); debugTimeStr != "" {
		parsedTime, err := time.Parse("2006-01-02T15:04", debugTimeStr)
		if err != nil {
			return QueryParams{}, fmt.Errorf("invalid 'debugNow' parameter format, expected YYYY-MM-DDTHH:MM")
		}
		debugNow = parsedTime
	}

	// Process the template parameter
	template := "default" // Default template
	if templateParam := r.URL.Query().Get("template"); templateParam != "" {
		// Check that the template parameter contains only alphanumeric characters
		validTemplate := true
		for _, char := range templateParam {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
				validTemplate = false
				break
			}
		}

		// Check if the template file exists
		if validTemplate {
			templateFile := fmt.Sprintf("template-%s.html", templateParam)
			if _, err := os.Stat(templateFile); err == nil {
				template = templateParam
			} else {
				log.Printf("Template file not found: %s", templateFile)
			}
		} else {
			log.Printf("Invalid template parameter (non-alphanumeric): %s", templateParam)
		}
	}

	return QueryParams{
		Location: location,
		Format:   format,
		Width:    width,
		Height:   height,
		DebugNow: debugNow,
		Template: template,
	}, nil
}

func addExpiryHeader(w http.ResponseWriter) {
	now := time.Now().UTC()
	minutes := now.Minute()
	var nextHalfHour int
	if minutes < 30 {
		nextHalfHour = 30
	} else {
		nextHalfHour = 60
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC).
		Add(time.Duration(nextHalfHour) * time.Minute)
	w.Header().Set("Expires", next.Format(http.TimeFormat))
}

func queryDatapoints(params QueryParams, now time.Time) ([]datapoint, error) {
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := startOfDay.Add(24 * time.Hour)

	startUTC := startOfDay.UTC()
	endUTC := endOfDay.UTC().Add(30 * time.Minute)

	log.Printf("Querying data between %s and %s", startUTC.Format(time.RFC3339), endUTC.Format(time.RFC3339))

	query := fmt.Sprintf(`
		from(bucket: "%s")
		|> range(start: %s, stop: %s)
		|> filter(fn: (r) => r._measurement == "electricity_pricing" and r.location == %q and r._field == "unit_price_inc_vat_price")
		|> aggregateWindow(every: 30m, fn: mean, createEmpty: false, timeSrc: "_start")
		|> yield(name: "mean")
	`, influxBucket, startUTC.Format(time.RFC3339), endUTC.Format(time.RFC3339), params.Location)

	client := influxdb2.NewClient(influxURL, influxToken)
	defer client.Close()

	queryAPI := client.QueryAPI(influxOrg)
	result, err := queryAPI.Query(context.Background(), query)
	if err != nil {
		return nil, fmt.Errorf("query error: %w", err)
	}

	points := []datapoint{}
	for result.Next() {
		record := result.Record()
		if v, ok := record.Value().(float64); ok {
			points = append(points, datapoint{
				Time:  record.Time(),
				Value: v,
			})
		}
	}
	if result.Err() != nil {
		return nil, fmt.Errorf("error parsing Influx result: %w", result.Err())
	}
	return points, nil
}

func renderHtml(w http.ResponseWriter, params QueryParams) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templateFile := fmt.Sprintf("template-%s.html", params.Template)
	tmpl, err := template.ParseFiles(templateFile)
	if err != nil {
		return fmt.Errorf("template error: %w", err)
	}

	// Create debugNowStr for the template if debugNow is provided
	var debugNowStr string
	if !params.DebugNow.IsZero() {
		debugNowStr = params.DebugNow.Format("2006-01-02T15:04")
	}

	data := struct {
		Location      string
		Width         int
		Height        int
		DisplayWidth  int
		DisplayHeight int
		DebugNowStr   string
	}{
		Location:      html.EscapeString(params.Location),
		Width:         params.Width,
		Height:        params.Height,
		DisplayWidth:  params.Width / 2,
		DisplayHeight: params.Height / 2,
		DebugNowStr:   debugNowStr,
	}
	if err := tmpl.Execute(w, data); err != nil {
		return fmt.Errorf("template execution error: %w", err)
	}
	return nil
}

func renderPng(w http.ResponseWriter, points []datapoint, width, height int, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered in renderPng: %v\n", r)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()

	const marginLeft = 40.0
	const marginBottom = 40.0
	const marginTop = 30.0
	const marginRight = 40.0
	const cornerRadius = 30
	const gradientStep = 8
	const yLabelInterval = 10.0
	const yMinNudge = 6
	const xNudge = 10.0

	dc := gg.NewContext(width, height)
	dc.SetRGB(1, 1, 1)
	dc.Clear()

	if len(points) == 0 {
		dc.SetRGB(0, 0, 0)
		dc.DrawStringAnchored("No data", float64(width)/2, float64(height)/2, 0.5, 0.5)
		outputPng(w, dc)
		return
	}

	// Timezone
	loc, _ := time.LoadLocation("Europe/London")
	for i := range points {
		points[i].Time = points[i].Time.In(loc)
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})

	// Find Y range
	maxY := 0.0
	minY := 0.0
	hasNegative := false

	for _, pt := range points {
		if pt.Value > maxY {
			maxY = pt.Value
		}
		if pt.Value < minY {
			minY = pt.Value
			hasNegative = true
		}
	}

	// Round up maxY to the nearest interval
	yMax := math.Ceil(maxY/yLabelInterval) * yLabelInterval
	if yMax < yLabelInterval {
		yMax = yLabelInterval
	}

	// Round down minY to the nearest 5 if there are negative values
	yMin := 0.0
	if hasNegative {
		yMin = math.Floor(minY/5) * 5
	}

	plotW := float64(width) - marginLeft - marginRight
	plotH := float64(height) - marginTop - marginBottom

	// Y-axis scaling factor (accounting for positive and negative values)
	yRange := yMax - yMin
	yScaleFactor := plotH / yRange

	// Thresholds
	thresholds := []struct {
		min, max float64
		color    color.NRGBA
	}{
		{math.Inf(-1), 0, color.NRGBA{105, 115, 191, 255}},
		{0, 10, color.NRGBA{115, 191, 105, 255}},
		{10, 20, color.NRGBA{234, 184, 57, 255}},
		{20, 30, color.NRGBA{242, 73, 92, 255}},
		{30, math.Inf(+1), color.NRGBA{196, 22, 42, 255}},
	}

	dc.DrawRoundedRectangle(marginLeft, marginTop, plotW, plotH, cornerRadius)
	dc.Clip()

	dc.SetColor(color.Black)
	dc.DrawRectangle(marginLeft, marginTop, plotW, plotH)
	dc.Fill()

	for _, t := range thresholds {
		// Clamp to chart range
		low := math.Min(t.max, yMax)
		high := math.Max(t.min, yMin)

		if low <= high {
			log.Printf("Skipping band min %.1f to max %.1f: invalid range", t.min, t.max)
			continue
		}

		y1 := float64(height) - marginBottom - ((low - yMin) * yScaleFactor)
		y2 := float64(height) - marginBottom - ((high - yMin) * yScaleFactor)

		if math.IsNaN(y1) || math.IsNaN(y2) {
			log.Printf("Skipping band %.1f–%.1f due to NaN", t.min, t.max)
			continue
		}

		dc.SetColor(color.NRGBA{t.color.R, t.color.G, t.color.B, 128})
		dc.DrawRectangle(marginLeft, y2, plotW, y1-y2)
		dc.Fill()
	}

	dc.ResetClip()

	if face, err := gg.LoadFontFace("Inter_18pt-Light.ttf", 16); err == nil {
		dc.SetFontFace(face)
	} else {
		log.Println("Failed to load SF Compact Light font:", err)
	}

	// Y-axis labels
	dc.SetColor(color.Gray{64})

	// Zero and positive labels
	for v := 0.0; v <= yMax; v += yLabelInterval {
		y := float64(height) - marginBottom - ((v - yMin) * yScaleFactor)
		labelXNudge, labelYNudge := 0.0, 0.0
		if v == 0 && !hasNegative {
			labelYNudge = yMinNudge
		}
		if v == yMax || (v == 0 && !hasNegative) {
			labelXNudge = xNudge
		}
		label := fmt.Sprintf("%.0fp", v)
		dc.DrawStringAnchored(label, marginLeft-8+labelXNudge, y-labelYNudge, 1, 0.5)
	}

	// Negative label (just the minimum)
	if hasNegative {
		y := float64(height) - marginBottom - ((yMin - yMin) * yScaleFactor)
		label := fmt.Sprintf("%.0fp", yMin)
		dc.DrawStringAnchored(label, marginLeft-8+xNudge, y-yMinNudge, 1, 0.5)
	}

	// X-axis labels
	startTime := time.Date(points[0].Time.Year(), points[0].Time.Month(), points[0].Time.Day(), 0, 0, 0, 0, loc)
	for hour := 0; hour <= 24; hour += 2 {
		t := startTime.Add(time.Duration(hour) * time.Hour)
		label := t.Format("3pm") // 12am, 3am, 6pm, etc.
		x := marginLeft + (float64(hour)/24)*plotW
		if hour == 0 {
			x += xNudge
		} else if hour == 24 {
			x -= xNudge
		}
		y := float64(height) - marginBottom + 20 // Increased offset to move labels further down
		dc.DrawStringAnchored(label, x, y, 0.5, 0)
	}

	// Find index of the closest point to now
	closestIndex := 0
	minDiff := math.MaxFloat64
	for i, pt := range points {
		diff := math.Abs(pt.Time.Sub(now).Seconds())
		if diff < minDiff {
			minDiff = diff
			closestIndex = i
		}
	}

	currentValue := points[closestIndex].Value

	var lineColor = color.NRGBA{0, 0, 0, 255}
	for _, t := range thresholds {
		if currentValue >= t.min && currentValue < t.max {
			lineColor = t.color
			break
		}
	}

	// Plot line
	dc.SetColor(lineColor)
	dc.SetLineWidth(3)
	dc.SetDash(5, 5)
	for i := 0; i < len(points)-1; i++ {
		x1 := marginLeft + (points[i].Time.Sub(startTime).Hours()/24)*plotW
		y1 := float64(height) - marginBottom - ((points[i].Value - yMin) * yScaleFactor)
		x2 := marginLeft + (points[i+1].Time.Sub(startTime).Hours()/24)*plotW
		y2 := float64(height) - marginBottom - ((points[i+1].Value - yMin) * yScaleFactor)
		alpha := uint8(255)
		if i < closestIndex {
			alpha = uint8(math.Max(255-(gradientStep*3*2)-float64(closestIndex-i)*gradientStep, 255-(gradientStep*8*2)))
		}
		dc.SetColor(color.NRGBA{lineColor.R, lineColor.G, lineColor.B, alpha})
		if i == closestIndex {
			dc.SetDash()
		}
		dc.DrawLine(x1, y1, x2, y2)
		dc.Stroke()
	}

	// Draw circle on current value
	if face, err := gg.LoadFontFace("Inter_18pt-SemiBold.ttf", 20); err == nil {
		dc.SetFontFace(face)
	} else {
		log.Println("Failed to load font for current value label:", err)
	}

	current := points[closestIndex]
	cx := marginLeft + (current.Time.Sub(startTime).Hours()/24.0)*plotW
	cy := float64(height) - marginBottom - ((current.Value - yMin) * yScaleFactor)

	// Draw circle
	dc.SetColor(lineColor)
	dc.DrawCircle(cx, cy, 5)
	dc.Fill()

	// Draw label slightly above the circle
	label := fmt.Sprintf("%.1fp", current.Value)
	textY := cy - 25
	if textY < marginTop+25 {
		textY = cy + 25 // Move below if too close to the top
	}

	// Adjust label position to ensure it stays within chart boundaries
	textX := cx + 25
	if textX+50 > float64(width)-marginRight { // Ensure it doesn't go off the right edge
		textX = cx - 50
	}
	if textX-50 < marginLeft { // Ensure it doesn't go off the left edge
		textX = cx + 50
	}

	dc.SetColor(color.White)
	dc.DrawStringAnchored(label, textX, textY, 0.5, 1)

	outputPng(w, dc)
}

func outputPng(w http.ResponseWriter, dc *gg.Context) {
	w.Header().Set("Content-Type", "image/png")
	err := dc.EncodePNG(w)
	if err != nil {
		log.Println("Failed to write PNG:", err)
		http.Error(w, "Failed to render image", http.StatusInternalServerError)
	}
}

func renderCsv(w http.ResponseWriter, points []datapoint) {
	w.Header().Set("Content-Type", "text/plain")
	csvWriter := csv.NewWriter(w)
	csvWriter.Comma = ','

	for _, dp := range points {
		csvWriter.Write([]string{dp.Time.Format(time.RFC3339), fmt.Sprintf("%.6f", dp.Value)})
	}
	csvWriter.Flush()
}
