package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"image/color"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"text/template"

	"time"

	"github.com/fogleman/gg"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type datapoint struct {
	Time  time.Time
	Value float64
}

type threshold struct {
	name     string
	min, max float64
	color    color.NRGBA
}

var (
	defaultHttpPort = "8080"
	defaultLogLevel = "INFO"
	logLevel        = os.Getenv("LOG_LEVEL")
	httpPort        = os.Getenv("HTTP_PORT")
	influxURL       = os.Getenv("INFLUXDB_URL")
	influxToken     = os.Getenv("INFLUXDB_TOKEN")
	influxOrg       = os.Getenv("INFLUXDB_ORG")
	influxBucket    = os.Getenv("INFLUXDB_BUCKET")
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

const defaultHtmlTemplate = "default"
const defaultWidth = 1014
const defaultHeight = 474
const marginLeft = 40.0
const marginBottom = 40.0
const marginTop = 30.0
const marginRight = 40.0
const yLabelPosInterval = 10
const yLabelNegInterval = 5
const yLabelDefaultXNudge = marginLeft - 6.0
const yLabelBoundaryXNudge = marginLeft + 3.0
const yLabelDefaultYNudge = 3.0
const yLabelBoundaryYNudge = 6.0
const xLabelDefaultXNudge = 0.0
const xLabelBoundaryXNudge = 6.0
const xLabelDefaultYNudge = 3.0
const nowTextDefaultNudge = 8.0

var (
	loc, _     = time.LoadLocation("Europe/London")
	labelColor = color.NRGBA{220, 220, 220, 255}
	gridColor  = color.NRGBA{112, 112, 112, 255}
	thresholds = []threshold{
		{"blue", math.Inf(-1), 0, color.NRGBA{105, 115, 191, 255}},
		{"green", 0, 10, color.NRGBA{115, 191, 105, 255}},
		{"yellow", 10, 20, color.NRGBA{234, 184, 57, 255}},
		{"light-red", 20, 30, color.NRGBA{242, 73, 92, 255}},
		{"dark-red", 30, math.Inf(+1), color.NRGBA{196, 22, 42, 255}},
	}
	axisFontFace, axisFontErr   = gg.LoadFontFace("Inter_18pt-Light.ttf", 16)
	labelFontFace, labelFontErr = gg.LoadFontFace("Inter_18pt-SemiBold.ttf", 20)
)

func init() {
	prometheus.MustRegister(httpRequests, httpDuration, queryCounter)
}

func main() {
	if logLevel == "" {
		logLevel = defaultLogLevel
	}
	if level, err := logrus.ParseLevel(logLevel); err == nil {
		logrus.SetLevel(level)
	} else {
		logrus.Fatalf("Invalid LOG_LEVEL: %s", logLevel)
	}
	if httpPort == "" {
		logrus.Debugf("No HTTP_PORT specified, defaulting to: %s", defaultHttpPort)
		httpPort = defaultHttpPort
	}
	if axisFontErr != nil {
		logrus.Fatalf("Failed to load axis font face: %v", axisFontErr)
	}
	if labelFontErr != nil {
		logrus.Fatalf("Failed to load axis font face: %v", axisFontErr)
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

	logrus.Infof("Starting server on port: %s", httpPort)
	logrus.Fatal(http.ListenAndServe(":"+httpPort, nil))
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
		logrus.Warnf("Failed to load UK timezone: %v", err)
		loc = time.UTC
	}

	now := time.Now().In(loc)
	if !params.DebugNow.IsZero() {
		logrus.Debugf("Overriding current time with debugNow parameter")
		now = params.DebugNow.In(loc)
	}
	logrus.Infof("Assuming now=%s", now.Format(time.RFC3339))

	points, err := queryDatapoints(params, now)
	if err != nil {
		logrus.Warnf("Failed to query datapoints: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	logrus.Trace("Adding expiry header")
	addExpiryHeader(w)

	format := params.Format
	switch params.Format {
	case "html":
		logrus.Info("Rendering HTML")
		if err := renderHtml(w, params); err != nil {
			logrus.Warnf("Failed to render HTML: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case "png":
		logrus.Info("Generating PNG image")
		renderPng(w, points, params.Width, params.Height, now)
	case "csv":
		logrus.Info("Generating CSV data")
		renderCsv(w, points)
	default:
		format = "invalid"
		http.Error(w, fmt.Errorf("invalid 'format' specified").Error(), http.StatusBadRequest)
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

	width := defaultWidth
	height := defaultHeight

	if wStr := r.URL.Query().Get("width"); wStr != "" {
		wVal, err := strconv.Atoi(wStr)
		if err != nil || wVal < 200 || wVal > 2000 {
			return QueryParams{}, fmt.Errorf("invalid 'width' query parameter")
		}
		logrus.Tracef("Overriding width to: %d", wVal)
		width = wVal
	}

	if hStr := r.URL.Query().Get("height"); hStr != "" {
		hVal, err := strconv.Atoi(hStr)
		if err != nil || hVal < 100 || hVal > 1200 {
			return QueryParams{}, fmt.Errorf("invalid 'height' query parameter")
		}
		logrus.Tracef("Overriding height to: %d", hVal)
		height = hVal
	}
	logrus.Debugf("Using image size: width=%d, height=%d", width, height)

	var debugNow time.Time
	if debugNowParam := r.URL.Query().Get("debugNow"); debugNowParam != "" {
		parsedTime, err := time.Parse("2006-01-02T15:04", debugNowParam)
		if err != nil {
			logrus.Warnf("Failed to parse 'debugNow' query parameter: %v", err)
			return QueryParams{}, fmt.Errorf("invalid 'debugNow' parameter format, expected YYYY-MM-DDTHH:mm")
		}
		debugNow = parsedTime
	}

	htmlTemplate := defaultHtmlTemplate
	if templateParam := r.URL.Query().Get("template"); templateParam != "" {
		if isAlphaNumeric(templateParam) {
			templateFile := fmt.Sprintf("template-%s.html", templateParam)
			if _, err := os.Stat(templateFile); err == nil {
				htmlTemplate = templateParam
			} else {
				logrus.Warnf("Template file not found: %s", templateFile)
			}
		} else {
			logrus.Warnf("Invalid template parameter (non-alphanumeric): %s", templateParam)
		}
	}

	return QueryParams{
		Location: location,
		Format:   format,
		Width:    width,
		Height:   height,
		DebugNow: debugNow,
		Template: htmlTemplate,
	}, nil
}

func isAlphaNumeric(templateParam string) bool {
	valid := true
	for _, char := range templateParam {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			valid = false
			break
		}
	}
	return valid
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
	endOfTomorrow := startOfDay.Add(48 * time.Hour)

	startUTC := startOfDay.UTC()
	endUTC := endOfTomorrow.UTC()

	logrus.Debugf("Querying data between: startUTC=%s, endUTC=%s", startUTC.Format(time.RFC3339), endUTC.Format(time.RFC3339))

	query := fmt.Sprintf(`
		from(bucket: "%s")
		|> range(start: %s, stop: %s)
		|> filter(fn: (r) => r._measurement == "electricity_pricing" and r.location == %q and r._field == "unit_price_inc_vat_price")
		|> aggregateWindow(every: 30m, fn: mean, createEmpty: false, timeSrc: "_start")
		|> yield(name: "mean")
	`, influxBucket, startUTC.Format(time.RFC3339), endUTC.Format(time.RFC3339), params.Location)
	logrus.Tracef("Running query=%s", query)

	client := influxdb2.NewClient(influxURL, influxToken)
	defer client.Close()

	queryAPI := client.QueryAPI(influxOrg)
	result, err := queryAPI.Query(context.Background(), query)
	if err != nil {
		logrus.Warnf("Failed to execute query: %v", err)
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
	sort.Slice(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})
	if result.Err() != nil {
		logrus.Warnf("Query result error: %v", result.Err())
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
			logrus.Warnf("Failed ot render PNG: %v", r)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()

	if len(points) == 0 {
		logrus.Info("No points found - rendering 'No data' message")
		dc := gg.NewContext(width, height)
		dc.SetRGB(0, 0, 0)
		dc.DrawStringAnchored("No data", float64(width)/2, float64(height)/2, 0.5, 0.5)
		outputPng(w, dc)
		return
	}

	minValue, maxValue, hasNegative := findValueRange(points)
	minY, maxY, yRange := calculatePlotRange(minValue, maxValue, hasNegative)
	logrus.Debugf("Calculated minValue=%.1f, maxValue=%.1f, minY=%d, maxY=%d, yRange=%d", minValue, maxValue, minY, maxY, yRange)

	todayPoints, tomorrowPoints := splitPoints(now, points)
	logrus.Debugf("len(todayPoints)=%d, len(tomorrowPoints)=%d", len(todayPoints), len(tomorrowPoints))

	plotW := width - (marginLeft + marginRight)
	plotH := height - (marginTop + marginBottom)
	yScaleFactor := float64(plotH) / float64(yRange)

	gradientStops := calculateGradientStops(plotH, minY, yRange, maxY)

	logrus.Debug("Drawing background")
	background := gg.NewContext(width, height)
	background.SetColor(color.Black)
	background.DrawRectangle(0, 0, float64(width), float64(height))
	background.Fill()

	logrus.Debug("Drawing y-axis labels")
	yLines := gg.NewContext(plotW, plotH)
	yLines.SetColor(gridColor)
	yLines.SetLineWidth(1)
	yLines.SetDash(3, 3)

	yLabels := gg.NewContext(marginLeft*2, height)
	yLabels.SetColor(labelColor)
	yLabels.SetFontFace(axisFontFace)

	for v := minY; v <= maxY; {
		y := float64(plotH) - (float64(v-minY) * yScaleFactor)
		yLines.DrawLine(0, y, float64(width), y)
		yLines.Stroke()

		yLabelXNudge, yLabelYNudge := yLabelDefaultXNudge, yLabelDefaultYNudge
		if v == minY {
			yLabelXNudge = yLabelBoundaryXNudge
			yLabelYNudge = yLabelBoundaryYNudge
		} else if v == maxY {
			yLabelXNudge = yLabelBoundaryXNudge
			yLabelYNudge = -yLabelBoundaryYNudge
		}
		label := fmt.Sprintf("%dp", v)
		yLabels.DrawStringAnchored(label, yLabelXNudge, marginTop+y-yLabelYNudge, 1, 0.5)

		if v < 0 {
			v += yLabelNegInterval
		} else {
			v += yLabelPosInterval
		}
	}

	logrus.Debug("Drawing x-axis labels")
	xLabels := gg.NewContext(width, marginBottom)
	xLabels.SetColor(labelColor)
	xLabels.SetFontFace(axisFontFace)

	startTime := time.Date(todayPoints[0].Time.Year(), todayPoints[0].Time.Month(), todayPoints[0].Time.Day(), 0, 0, 0, 0, loc)
	for hour := 0; hour <= 24; hour += 2 {
		x := marginLeft + (float64(hour)/24)*float64(plotW)
		xLabelXNudge, xLabelYNudge := xLabelDefaultXNudge, xLabelDefaultYNudge
		if hour == 0 {
			xLabelXNudge = xLabelBoundaryXNudge
		} else if hour == 24 {
			xLabelXNudge = -xLabelBoundaryXNudge
		}
		xLabels.DrawStringAnchored(startTime.Add(time.Duration(hour)*time.Hour).Format("3pm"), x+xLabelXNudge, xLabelYNudge, 0.5, 1)
	}

	nowValue, nowX, nowY := calculateNowValues(todayPoints, now, plotW, plotH, minY, yScaleFactor)
	logrus.Debugf("Calculated nowValue=%.1f, nowX=%.1f, nowY=%.1f", nowValue, nowX, nowY)

	logrus.Debugf("Drawing today's line")
	todayLineMask := gg.NewContext(plotW, plotH)
	todayLineMask.SetColor(color.White)
	todayLineMask.SetLineWidth(8)
	drawLine(todayPoints, plotW, plotH, minY, yScaleFactor, todayLineMask)

	todayLine := gg.NewContext(plotW, plotH)
	err := todayLine.SetMask(todayLineMask.AsMask())
	if err != nil {
		logrus.Warnf("Error setting todayLineMask: %v", err)
	}
	todayLine.SetFillStyle(gradientStops)
	todayLine.DrawRectangle(0, 0, float64(plotW), float64(plotH))
	todayLine.Fill()

	todayLineAlphaMask := gg.NewContext(plotW, plotH)
	todayLineAlphaMask.SetColor(color.NRGBA{255, 255, 255, 128})
	todayLineAlphaMask.DrawRectangle(0, 0, nowX, float64(plotH))
	todayLineAlphaMask.Fill()
	todayLineAlphaMask.SetColor(color.White)
	todayLineAlphaMask.DrawRectangle(nowX, 0, float64(plotW)-nowX, float64(plotH))
	todayLineAlphaMask.Fill()

	todayLineWithAlpha := gg.NewContext(plotW, plotH)
	err = todayLineWithAlpha.SetMask(todayLineAlphaMask.AsMask())
	if err != nil {
		logrus.Warnf("Error setting todayLineWithAlpha mask: %v", err)
	}
	todayLineWithAlpha.DrawImage(todayLine.Image(), 0, 0)

	logrus.Debugf("Drawing tomorrow's line")
	tomorrowLineMask := gg.NewContext(plotW, plotH)
	tomorrowLineMask.SetColor(color.NRGBA{255, 255, 255, 204})
	tomorrowLineMask.SetLineWidth(2)
	drawLine(tomorrowPoints, plotW, plotH, minY, yScaleFactor, tomorrowLineMask)

	tomorrowLine := gg.NewContext(plotW, plotH)
	err = tomorrowLine.SetMask(tomorrowLineMask.AsMask())
	if err != nil {
		logrus.Warnf("Error setting tomorrowLine mask: %v", err)
	}
	tomorrowLine.SetFillStyle(gradientStops)
	tomorrowLine.DrawRectangle(0, 0, float64(plotW), float64(plotH))
	tomorrowLine.Fill()

	logrus.Debugf("Drawing now point and label")
	nowLabel := gg.NewContext(plotW, plotH)
	nowLabel.SetFontFace(labelFontFace)
	nowLabel.SetColor(color.White)
	nowLabel.DrawCircle(nowX, nowY, 8)
	nowLabel.Fill()

	nowTextXNudge, nowTextYNudge := nowTextDefaultNudge, -nowTextDefaultNudge
	nowTextAX, nowTextAY := 0.0, 0.0
	if (float64(plotW) - nowX) < marginLeft {
		logrus.Trace("Now label is too close to right edge - swapping it to left of point")
		nowTextXNudge = -nowTextXNudge
		nowTextAX = 1
	}
	if nowY < marginTop {
		logrus.Trace("Now label is too close to the top edge - swapping it to below the point")
		nowTextYNudge = -nowTextYNudge
		nowTextAY = 1
	}

	nowLabel.SetColor(color.White)
	nowLabel.DrawStringAnchored(fmt.Sprintf("%.1fp", nowValue), nowX+nowTextXNudge, nowY+nowTextYNudge, nowTextAX, nowTextAY)

	logrus.Debug("Assembling final image")
	dc := gg.NewContext(width, height)
	dc.DrawImage(background.Image(), 0, 0)
	dc.DrawImage(yLines.Image(), marginLeft, marginTop)
	dc.DrawImage(tomorrowLine.Image(), marginLeft, marginTop)
	dc.DrawImage(todayLineWithAlpha.Image(), marginLeft, marginTop)
	dc.DrawImage(nowLabel.Image(), marginLeft, marginTop)
	dc.DrawImage(xLabels.Image(), 0, marginTop+plotH)
	dc.DrawImage(yLabels.Image(), 0, 0)

	logrus.Debug("Outputting PNG")
	outputPng(w, dc)
}

func calculateNowValues(todayPoints []datapoint, now time.Time, plotW int, plotH int, minY int, yScaleFactor float64) (float64, float64, float64) {
	nowIndex := 0
	for i := 0; i < len(todayPoints); i++ {
		if todayPoints[i].Time.Compare(now) > 0 {
			break
		}
		nowIndex = i
	}
	logrus.Tracef("Calculated nowIndex=%d", nowIndex)

	nowPoint := todayPoints[nowIndex]
	nowValue := nowPoint.Value
	nowX := float64(nowIndex) / 48.0 * float64(plotW)
	nowY := float64(plotH) - ((nowValue - float64(minY)) * yScaleFactor)
	return nowValue, nowX, nowY
}

func calculateGradientStops(plotH int, yMin int, yRange int, yMax int) gg.Gradient {
	gradientStops := gg.NewLinearGradient(0, float64(plotH), 0, 0)
	yPos := yMin
	for _, t := range thresholds {
		if t.max < float64(yPos) {
			logrus.Tracef("Skipping %s band %.1f to %.1f as yPos is outside range: %d", t.name, t.min, t.max, yPos)
			continue
		}

		if yPos == yMin {
			stop := 0.0
			logrus.Tracef("Adding first color stop at yPos=%d, stop=%.2f: %s", yPos, stop, t.name)
			gradientStops.AddColorStop(stop, t.color)
		} else {
			yPos = int(t.min) + 2
			stop := float64(yPos-yMin) / float64(yRange)
			logrus.Tracef("Adding color stop start at yPos=%d, stop=%.2f: %s", yPos, stop, t.name)
			gradientStops.AddColorStop(stop, t.color)
		}

		if t.max >= float64(yMax) {
			yPos = yMax
			stop := 1.0
			logrus.Tracef("Adding last color stop at yPos=%d, stop=%.2f: %s", yPos, stop, t.name)
			gradientStops.AddColorStop(stop, t.color)
		} else {
			yPos = int(t.max) - 2
			stop := float64(yPos-yMin) / float64(yRange)
			logrus.Tracef("Adding color stop end at yPos=%d, stop=%.2f: %s", yPos, stop, t.name)
			gradientStops.AddColorStop(stop, t.color)
		}
	}
	return gradientStops
}

func drawLine(points []datapoint, plotW int, plotH int, minY int, yScaleFactor float64, lineContext *gg.Context) {
	if len(points) > 1 {
		startTime := points[0].Time
		for i := 0; i < len(points)-1; i++ {
			x1 := (points[i].Time.Sub(startTime).Hours() / 24) * float64(plotW)
			y1 := float64(plotH) - ((points[i].Value - float64(minY)) * yScaleFactor)
			x2 := (points[i+1].Time.Sub(startTime).Hours() / 24) * float64(plotW)
			y2 := float64(plotH) - ((points[i+1].Value - float64(minY)) * yScaleFactor)
			logrus.Tracef("Drawing line from %.1f, %.1f to %.1f, %.1f", x1, y1, x2, y2)
			lineContext.DrawLine(x1, y1, x2, y2)
			lineContext.Stroke()
		}
	} else {
		logrus.Warnf("Not enough data to draw line")
	}
}

func splitPoints(now time.Time, points []datapoint) ([]datapoint, []datapoint) {
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
	todayPoints := make([]datapoint, 0)
	tomorrowPoints := make([]datapoint, 0)
	for _, pt := range points {
		pt.Time = pt.Time.In(loc)
		if pt.Time.Before(todayEnd) || pt.Time.Equal(todayEnd) {
			todayPoints = append(todayPoints, pt)
		}
		if pt.Time.Equal(todayEnd) || pt.Time.After(todayEnd) {
			tomorrowPoints = append(tomorrowPoints, pt)
		}
	}
	return todayPoints, tomorrowPoints
}

func findValueRange(points []datapoint) (float64, float64, bool) {
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
	return minY, maxY, hasNegative
}

func calculatePlotRange(minY float64, maxY float64, hasNegative bool) (int, int, int) {
	yMax := int(math.Ceil(maxY/float64(yLabelPosInterval))) * yLabelPosInterval
	yMin := 0
	if hasNegative {
		yMin = int(math.Floor(minY/yLabelNegInterval)) * yLabelNegInterval
	}
	yRange := yMax - yMin
	return yMin, yMax, yRange
}

func outputPng(w http.ResponseWriter, dc *gg.Context) {
	w.Header().Set("Content-Type", "image/png")
	if err := dc.EncodePNG(w); err != nil {
		logrus.Warnf("Failed to encode PNG: %v", err)
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
