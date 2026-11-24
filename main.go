// wxwarn — NWS Weather Warnings CLI
//
// Fetches active weather alerts, current conditions, and forecasts from the
// National Weather Service public API (api.weather.gov) for any US location.
// Geocoding is handled via OpenStreetMap Nominatim (no API key required).
//
// Usage:
//
//	wxwarn                          # Bozeman, MT — alerts only
//	wxwarn -daily                   # alerts + current conditions + today's forecast
//	wxwarn -weekly                  # alerts + current conditions + 7-day forecast
//	wxwarn -loc "Missoula, MT"      # different location
//	wxwarn -v -weekly               # verbose: full alert text + detailed narrative
//	wxwarn -no-color                # plain text output for piping / cron
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	// nwsBase is the root URL for all National Weather Service API calls.
	nwsBase = "https://api.weather.gov"

	// userAgent is required by the NWS API; requests without it may be blocked.
	userAgent = "wxwarn/1.0 (weather warnings CLI)"

	// ANSI color/style escape codes used for terminal output.
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorBlue   = "\033[34m"
	colorBold   = "\033[1m"
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorWhite  = "\033[97m"
)

// ── NWS Alert types ───────────────────────────────────────────────────────────

// AlertResponse is the top-level GeoJSON response from the NWS alerts endpoint.
type AlertResponse struct {
	Features []AlertFeature `json:"features"`
}

// AlertFeature wraps each individual alert in the GeoJSON feature array.
type AlertFeature struct {
	Properties AlertProperties `json:"properties"`
}

// AlertProperties contains the metadata for a single weather alert.
type AlertProperties struct {
	ID          string    `json:"id"`
	Event       string    `json:"event"`       // e.g. "Winter Storm Warning"
	Headline    string    `json:"headline"`    // one-line summary
	Description string    `json:"description"` // full text body
	Instruction string    `json:"instruction"` // recommended public actions
	Severity    string    `json:"severity"`    // Extreme | Severe | Moderate | Minor
	Urgency     string    `json:"urgency"`
	Certainty   string    `json:"certainty"`
	Status      string    `json:"status"` // "actual" (vs. test/exercise)
	Effective   time.Time `json:"effective"`
	Expires     time.Time `json:"expires"`
	AreaDesc    string    `json:"areaDesc"`   // human-readable affected zone
	SenderName  string    `json:"senderName"` // issuing NWS office
}

// ── NWS Points / Forecast types ───────────────────────────────────────────────

// PointsResponse is returned by /points/{lat},{lon} and provides grid metadata
// including the forecast URL and the nearest city name.
type PointsResponse struct {
	Properties PointsProperties `json:"properties"`
}

// PointsProperties holds the fields we care about from the points endpoint.
type PointsProperties struct {
	// Forecast is the full URL to the 7-day forecast for this grid cell.
	Forecast string `json:"forecast"`

	// ObservationStations is the URL listing nearby ASOS/AWOS weather stations.
	ObservationStations string `json:"observationStations"`

	RelativeLocation struct {
		Properties struct {
			City  string `json:"city"`
			State string `json:"state"`
		} `json:"properties"`
	} `json:"relativeLocation"`
}

// ForecastResponse is the response envelope from the NWS forecast endpoint.
type ForecastResponse struct {
	Properties ForecastProperties `json:"properties"`
}

// ForecastProperties contains the last-updated timestamp and all forecast periods.
type ForecastProperties struct {
	Updated time.Time        `json:"updated"`
	Periods []ForecastPeriod `json:"periods"`
}

// ForecastPeriod represents a single named forecast period (e.g. "Thursday Night").
type ForecastPeriod struct {
	Number          int       `json:"number"`
	Name            string    `json:"name"`
	StartTime       time.Time `json:"startTime"`
	IsDaytime       bool      `json:"isDaytime"`
	Temperature     int       `json:"temperature"`
	TemperatureUnit string    `json:"temperatureUnit"` // "F" or "C"
	WindSpeed       string    `json:"windSpeed"`
	WindDirection   string    `json:"windDirection"`
	ShortForecast   string    `json:"shortForecast"`   // e.g. "Partly Sunny"
	DetailedForecast string   `json:"detailedForecast"` // verbose narrative
	ProbabilityOfPrecipitation struct {
		Value *int `json:"value"` // pointer so we can detect null
	} `json:"probabilityOfPrecipitation"`
}

// ── NWS Observation Station types ────────────────────────────────────────────

// StationsResponse is the GeoJSON feature collection returned by the
// observationStations URL obtained from the /points endpoint.
type StationsResponse struct {
	Features []StationFeature `json:"features"`
}

// StationFeature wraps a single observation station in the collection.
type StationFeature struct {
	Properties StationProperties `json:"properties"`
}

// StationProperties contains the station identifier and human-readable name.
type StationProperties struct {
	StationIdentifier string  `json:"stationIdentifier"` // e.g. "KBZN"
	Name              string  `json:"name"`               // e.g. "Bozeman Yellowstone International Airport"
	Elevation         struct {
		Value float64 `json:"value"` // metres
	} `json:"elevation"`
}

// ── NWS Current Observation types ────────────────────────────────────────────

// ObservationResponse is the GeoJSON response from
// /stations/{stationId}/observations/latest.
type ObservationResponse struct {
	Properties ObservationProperties `json:"properties"`
}

// ObservationProperties holds every measured variable from the latest METAR.
// NWS returns most values as an object with "value" (float64 or null) and "unitCode".
type ObservationProperties struct {
	Timestamp           time.Time       `json:"timestamp"`
	TextDescription     string          `json:"textDescription"` // e.g. "Overcast"
	Temperature         NWSMeasurement  `json:"temperature"`         // °C
	Dewpoint            NWSMeasurement  `json:"dewpoint"`            // °C
	WindDirection       NWSMeasurement  `json:"windDirection"`       // degrees
	WindSpeed           NWSMeasurement  `json:"windSpeed"`           // km/h
	WindGust            NWSMeasurement  `json:"windGust"`            // km/h (often null)
	Visibility          NWSMeasurement  `json:"visibility"`          // metres
	RelativeHumidity    NWSMeasurement  `json:"relativeHumidity"`    // %
	WindChill           NWSMeasurement  `json:"windChill"`           // °C
	HeatIndex           NWSMeasurement  `json:"heatIndex"`           // °C
	BarometricPressure  NWSMeasurement  `json:"barometricPressure"`  // Pa
	PresentWeather      []struct {
		Intensity string `json:"intensity"`
		Weather   string `json:"weather"`
	} `json:"presentWeather"`
}

// NWSMeasurement represents a nullable measurement returned by the NWS API.
// The Value field is a pointer so we can distinguish "0" from "null".
type NWSMeasurement struct {
	Value    *float64 `json:"value"`
	UnitCode string   `json:"unitCode"`
}

// ── Geocoding ─────────────────────────────────────────────────────────────────

// GeoResult is one entry in the JSON array returned by Nominatim.
type GeoResult struct {
	Lat         string `json:"lat"`
	Lon         string `json:"lon"`
	DisplayName string `json:"display_name"`
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

// httpGet performs an HTTP GET with NWS-required headers and up to 3 retries
// with exponential back-off on transient network failures.
func httpGet(reqURL string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		// NWS requires a descriptive User-Agent; it uses this for rate-limiting.
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/geo+json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 3 {
				fmt.Fprintf(os.Stderr, "  retrying (%d/3)...\n", attempt+1)
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, reqURL)
		}
		return io.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("request failed after 3 attempts: %w", lastErr)
}

// geocode converts a city/state string or US zip code into WGS84 coordinates
// using the OpenStreetMap Nominatim API (no API key required).
func geocode(place string) (float64, float64, string, error) {
	q := url.QueryEscape(place + ", USA")
	apiURL := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1&countrycodes=us", q)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return 0, 0, "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, "", err
	}

	var results []GeoResult
	if err := json.Unmarshal(body, &results); err != nil || len(results) == 0 {
		return 0, 0, "", fmt.Errorf("could not geocode %q — try a more specific location", place)
	}

	var lat, lon float64
	fmt.Sscanf(results[0].Lat, "%f", &lat)
	fmt.Sscanf(results[0].Lon, "%f", &lon)
	return lat, lon, results[0].DisplayName, nil
}

// getNWSPoints calls the NWS /points/{lat},{lon} endpoint to retrieve the
// forecast grid URL, nearest observation-station list URL, and closest city name.
func getNWSPoints(lat, lon float64) (PointsProperties, error) {
	apiURL := fmt.Sprintf("%s/points/%.4f,%.4f", nwsBase, lat, lon)
	body, err := httpGet(apiURL)
	if err != nil {
		return PointsProperties{}, fmt.Errorf("NWS points lookup failed: %w", err)
	}
	var pr PointsResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return PointsProperties{}, err
	}
	return pr.Properties, nil
}

// getAlerts fetches all currently active NWS alerts for the given coordinates.
func getAlerts(lat, lon float64) ([]AlertFeature, error) {
	apiURL := fmt.Sprintf("%s/alerts/active?point=%.4f,%.4f&status=actual", nwsBase, lat, lon)
	body, err := httpGet(apiURL)
	if err != nil {
		return nil, err
	}
	var ar AlertResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, err
	}
	return ar.Features, nil
}

// getForecast fetches the 7-day forecast from the URL provided by getNWSPoints.
func getForecast(forecastURL string) (ForecastProperties, error) {
	body, err := httpGet(forecastURL)
	if err != nil {
		return ForecastProperties{}, err
	}
	var fr ForecastResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return ForecastProperties{}, err
	}
	return fr.Properties, nil
}

// getNearestStation fetches the list of observation stations from the NWS
// grid and returns the identifier and name of the first (closest) station.
func getNearestStation(stationsURL string) (string, string, error) {
	body, err := httpGet(stationsURL)
	if err != nil {
		return "", "", fmt.Errorf("could not fetch station list: %w", err)
	}
	var sr StationsResponse
	if err := json.Unmarshal(body, &sr); err != nil || len(sr.Features) == 0 {
		return "", "", fmt.Errorf("no observation stations found")
	}
	p := sr.Features[0].Properties
	return p.StationIdentifier, p.Name, nil
}

// getCurrentObservation fetches the most recent METAR observation from a
// specific NWS observation station (e.g. "KBZN").
func getCurrentObservation(stationID string) (ObservationProperties, error) {
	apiURL := fmt.Sprintf("%s/stations/%s/observations/latest", nwsBase, stationID)
	body, err := httpGet(apiURL)
	if err != nil {
		return ObservationProperties{}, fmt.Errorf("observation fetch failed: %w", err)
	}
	var obs ObservationResponse
	if err := json.Unmarshal(body, &obs); err != nil {
		return ObservationProperties{}, err
	}
	return obs.Properties, nil
}

// ── Unit conversion helpers ───────────────────────────────────────────────────

// celsiusToF converts Celsius to Fahrenheit.
func celsiusToF(c float64) float64 { return c*9.0/5.0 + 32.0 }

// metersToMiles converts metres to statute miles.
func metersToMiles(m float64) float64 { return m / 1609.344 }

// kmhToMph converts kilometres per hour to miles per hour.
func kmhToMph(k float64) float64 { return k * 0.621371 }

// windDegToCompass converts a wind direction in degrees (0–360) to an
// 8-point compass abbreviation (N, NE, E, SE, S, SW, W, NW).
func windDegToCompass(deg float64) string {
	dirs := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
	// Each octant is 45°; offset by 22.5° so 0° (N) lands in the middle of its bin.
	idx := int(math.Round(deg/45.0)) % 8
	return dirs[idx]
}

// ── Display helpers ───────────────────────────────────────────────────────────

// severityColor returns an ANSI color string appropriate for the given NWS
// alert severity level.
func severityColor(severity string) string {
	switch strings.ToLower(severity) {
	case "extreme":
		return colorRed + colorBold
	case "severe":
		return colorRed
	case "moderate":
		return colorYellow
	case "minor":
		return colorCyan
	default:
		return colorGray
	}
}

// severityIcon returns a Unicode emoji appropriate for the given NWS alert
// severity level.
func severityIcon(severity string) string {
	switch strings.ToLower(severity) {
	case "extreme":
		return "🚨"
	case "severe":
		return "⚠️ "
	case "moderate":
		return "🔶"
	case "minor":
		return "🔵"
	default:
		return "ℹ️ "
	}
}

// weatherIcon returns a Unicode emoji that represents the given short-forecast
// string, taking into account whether it is daytime or nighttime.
func weatherIcon(forecast string, daytime bool) string {
	f := strings.ToLower(forecast)
	switch {
	case strings.Contains(f, "blizzard"):
		return "❄️🌬️"
	case strings.Contains(f, "thunder"):
		return "⛈️ "
	case strings.Contains(f, "snow") && strings.Contains(f, "rain"):
		return "🌨️ "
	case strings.Contains(f, "snow") || strings.Contains(f, "blowing snow"):
		return "❄️ "
	case strings.Contains(f, "sleet") || strings.Contains(f, "freezing"):
		return "🌨️ "
	case strings.Contains(f, "rain") || strings.Contains(f, "shower"):
		return "🌧️ "
	case strings.Contains(f, "drizzle"):
		return "🌦️ "
	case strings.Contains(f, "fog"):
		return "🌫️ "
	case strings.Contains(f, "windy") || strings.Contains(f, "breezy"):
		return "💨 "
	case strings.Contains(f, "partly") || strings.Contains(f, "mostly cloudy"):
		if daytime {
			return "⛅ "
		}
		return "🌥️ "
	case strings.Contains(f, "cloudy") || strings.Contains(f, "overcast"):
		return "☁️ "
	case strings.Contains(f, "sunny") || strings.Contains(f, "clear"):
		if daytime {
			return "☀️ "
		}
		return "🌙 "
	default:
		if daytime {
			return "🌤️ "
		}
		return "🌙 "
	}
}

// tempColor returns an ANSI color code that reflects how hot or cold a given
// temperature feels. Internally normalises to Fahrenheit for comparison.
func tempColor(temp int, unit string) string {
	t := temp
	if unit == "C" {
		t = t*9/5 + 32
	}
	switch {
	case t >= 95:
		return colorRed
	case t >= 80:
		return colorYellow
	case t >= 60:
		return colorGreen
	case t >= 40:
		return colorCyan
	default:
		return colorBlue
	}
}

// tempColorF is the same as tempColor but accepts a float64 already in °F.
func tempColorF(tempF float64) string {
	switch {
	case tempF >= 95:
		return colorRed
	case tempF >= 80:
		return colorYellow
	case tempF >= 60:
		return colorGreen
	case tempF >= 40:
		return colorCyan
	default:
		return colorBlue
	}
}

// printAlert prints a formatted summary (and optionally the full text) of one
// NWS alert to standard output.
func printAlert(a AlertProperties, verbose bool, c func(string) string) {
	color := severityColor(a.Severity)
	icon := severityIcon(a.Severity)

	fmt.Printf("%s%s%s %s%s%s\n", c(color), icon, c(colorReset), c(color), a.Event, c(colorReset))
	fmt.Printf("  %sSeverity:%s  %s  |  %sUrgency:%s %s  |  %sCertainty:%s %s\n",
		c(colorBold), c(colorReset), a.Severity,
		c(colorBold), c(colorReset), a.Urgency,
		c(colorBold), c(colorReset), a.Certainty)
	fmt.Printf("  %sArea:%s      %s\n", c(colorBold), c(colorReset), a.AreaDesc)
	fmt.Printf("  %sEffective:%s %s\n", c(colorBold), c(colorReset), a.Effective.Local().Format("Mon Jan 2, 3:04 PM MST"))
	fmt.Printf("  %sExpires:%s   %s\n", c(colorBold), c(colorReset), a.Expires.Local().Format("Mon Jan 2, 3:04 PM MST"))
	fmt.Printf("  %sIssued by:%s %s\n", c(colorBold), c(colorReset), a.SenderName)

	if verbose {
		if a.Headline != "" {
			fmt.Printf("\n  %sHeadline:%s\n  %s\n", c(colorBold), c(colorReset), wordWrap(a.Headline, 72, "  "))
		}
		if a.Description != "" {
			fmt.Printf("\n  %sDescription:%s\n%s\n", c(colorBold), c(colorReset), wordWrap(a.Description, 72, "  "))
		}
		if a.Instruction != "" {
			fmt.Printf("\n  %sInstructions:%s\n%s\n", c(colorBold), c(colorReset), wordWrap(a.Instruction, 72, "  "))
		}
	} else if a.Headline != "" {
		fmt.Printf("\n  %s\n", wordWrap(a.Headline, 72, "  "))
	}
}

// printCurrentConditions displays the latest METAR observation in a compact,
// human-readable format with optional colour. It gracefully skips any fields
// whose values are null in the API response.
func printCurrentConditions(obs ObservationProperties, stationName string, c func(string) string) {
	// ── Sky / description ──
	desc := obs.TextDescription
	if desc == "" {
		desc = "N/A"
	}
	icon := weatherIcon(desc, true) // use daytime icon for current conditions
	fmt.Printf("  %sSky:%s        %s %s\n", c(colorBold), c(colorReset), icon, desc)

	// ── Temperature ──
	if obs.Temperature.Value != nil {
		tempF := celsiusToF(*obs.Temperature.Value)
		tc := tempColorF(tempF)
		fmt.Printf("  %sTemp:%s       %s%.0f°F%s", c(colorBold), c(colorReset), c(tc), tempF, c(colorReset))

		// Append feels-like (wind chill or heat index) when available
		if obs.WindChill.Value != nil {
			fcF := celsiusToF(*obs.WindChill.Value)
			fmt.Printf("  (feels like %s%.0f°F%s wind chill)", c(tempColorF(fcF)), fcF, c(colorReset))
		} else if obs.HeatIndex.Value != nil {
			hiF := celsiusToF(*obs.HeatIndex.Value)
			fmt.Printf("  (feels like %s%.0f°F%s heat index)", c(tempColorF(hiF)), hiF, c(colorReset))
		}
		fmt.Println()
	}

	// ── Dewpoint / Humidity ──
	if obs.Dewpoint.Value != nil {
		dpF := celsiusToF(*obs.Dewpoint.Value)
		fmt.Printf("  %sDewpoint:%s   %.0f°F", c(colorBold), c(colorReset), dpF)
		if obs.RelativeHumidity.Value != nil {
			fmt.Printf("  %sHumidity:%s %.0f%%", c(colorBold), c(colorReset), *obs.RelativeHumidity.Value)
		}
		fmt.Println()
	} else if obs.RelativeHumidity.Value != nil {
		fmt.Printf("  %sHumidity:%s  %.0f%%\n", c(colorBold), c(colorReset), *obs.RelativeHumidity.Value)
	}

	// ── Wind ──
	if obs.WindSpeed.Value != nil {
		windMph := kmhToMph(*obs.WindSpeed.Value)
		windDir := ""
		if obs.WindDirection.Value != nil {
			windDir = windDegToCompass(*obs.WindDirection.Value) + " "
		}

		fmt.Printf("  %sWind:%s       %s%.0f mph", c(colorBold), c(colorReset), windDir, windMph)

		// Append gust speed when reported
		if obs.WindGust.Value != nil && *obs.WindGust.Value > 0 {
			gustMph := kmhToMph(*obs.WindGust.Value)
			fmt.Printf("  (gusts %s%.0f mph%s)", c(colorYellow), gustMph, c(colorReset))
		}
		fmt.Println()
	}

	// ── Visibility ──
	if obs.Visibility.Value != nil {
		visMiles := metersToMiles(*obs.Visibility.Value)
		visColor := colorGreen
		if visMiles < 1 {
			visColor = colorRed // dense fog / very low visibility
		} else if visMiles < 3 {
			visColor = colorYellow
		}
		fmt.Printf("  %sVisibility:%s %s%.1f mi%s\n", c(colorBold), c(colorReset), c(visColor), visMiles, c(colorReset))
	}

	// ── Barometric Pressure ──
	if obs.BarometricPressure.Value != nil {
		// NWS reports pressure in Pascals; convert to inHg (1 inHg = 3386.39 Pa)
		inHg := *obs.BarometricPressure.Value / 3386.39
		fmt.Printf("  %sPressure:%s   %.2f inHg\n", c(colorBold), c(colorReset), inHg)
	}

	// ── Observation time and station ──
	fmt.Printf("  %sObserved:%s   %s  (%s%s%s)\n",
		c(colorBold), c(colorReset),
		obs.Timestamp.Local().Format("Mon Jan 2, 3:04 PM MST"),
		c(colorGray), stationName, c(colorReset))
}

// printForecastPeriod prints a single forecast period (e.g. "Thursday Night")
// with temperature, wind, precipitation probability, and—if verbose—the full
// narrative.
func printForecastPeriod(p ForecastPeriod, verbose bool, c func(string) string) {
	icon := weatherIcon(p.ShortForecast, p.IsDaytime)
	tc := tempColor(p.Temperature, p.TemperatureUnit)

	// Pad name to 16 chars for column alignment
	label := fmt.Sprintf("%-16s", p.Name)

	// Only show precipitation probability when non-zero
	precip := ""
	if p.ProbabilityOfPrecipitation.Value != nil && *p.ProbabilityOfPrecipitation.Value > 0 {
		precip = fmt.Sprintf("  %s💧 %d%%%s", c(colorCyan), *p.ProbabilityOfPrecipitation.Value, c(colorReset))
	}

	fmt.Printf("  %s%s%s  %s  %s%d°%s%s  %s%s%s  💨 %s %s%s\n",
		c(colorBold), label, c(colorReset),
		icon,
		c(tc), p.Temperature, p.TemperatureUnit, c(colorReset),
		c(colorGray), p.ShortForecast, c(colorReset),
		p.WindSpeed, p.WindDirection,
		precip,
	)

	if verbose && p.DetailedForecast != "" {
		fmt.Printf("%s\n", wordWrap(p.DetailedForecast, 72, "    "))
	}
}

// wordWrap reflows a multi-paragraph block of text to the given column width,
// preserving paragraph breaks (blank lines) and prefixing every line with indent.
func wordWrap(text string, width int, indent string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(text, "\n\n")
	var result strings.Builder

	for i, para := range paragraphs {
		para = strings.Join(strings.Fields(para), " ")
		words := strings.Fields(para)
		line := indent
		for _, w := range words {
			if len(line)+len(w)+1 > width+len(indent) && line != indent {
				result.WriteString(line + "\n")
				line = indent + w
			} else if line == indent {
				line += w
			} else {
				line += " " + w
			}
		}
		if line != indent {
			result.WriteString(line)
		}
		if i < len(paragraphs)-1 {
			result.WriteString("\n\n")
		}
	}
	return result.String()
}

// divider prints a horizontal rule using the gray color (or plain dashes when
// color is disabled).
func divider(c func(string) string) {
	fmt.Printf("%s%s%s\n", c(colorGray), strings.Repeat("─", 60), c(colorReset))
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	// ── Flags ──
	location := flag.String("loc", "Bozeman, MT", "Location to check (city, state or zip code)")
	verbose  := flag.Bool("v", false, "Show full alert text and detailed forecast narrative")
	daily    := flag.Bool("daily", false, "Show current conditions + today's forecast (daytime + tonight)")
	weekly   := flag.Bool("weekly", false, "Show current conditions + the full 7-day forecast")
	noColor  := flag.Bool("no-color", false, "Disable color output (useful for piping/logging)")
	flag.Parse()

	// Positional argument overrides -loc, e.g.: wxwarn "Great Falls, MT"
	if flag.NArg() > 0 {
		*location = strings.Join(flag.Args(), " ")
	}

	// -weekly is a superset of -daily; enabling both avoids duplicate logic below.
	if *weekly {
		*daily = true
	}

	// c() wraps an ANSI code so it can be stripped globally via -no-color.
	c := func(code string) string {
		if *noColor {
			return ""
		}
		return code
	}

	// ── Banner ──
	fmt.Printf("%s⛈  wxwarn%s — %s%s%s\n\n",
		c(colorBold), c(colorReset), c(colorCyan), *location, c(colorReset))

	// ── Geocode the requested location ──
	lat, lon, fullName, err := geocode(*location)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// ── Resolve NWS grid cell (needed for forecast URL + station list) ──
	points, pointsErr := getNWSPoints(lat, lon)
	nwsLoc := fullName // fall back to Nominatim name if NWS city lookup fails
	if pointsErr == nil {
		city := points.RelativeLocation.Properties.City
		state := points.RelativeLocation.Properties.State
		if city != "" {
			nwsLoc = fmt.Sprintf("%s, %s", city, state)
		}
	}

	fmt.Printf("%sLocation:%s %s (%.4f, %.4f)\n",
		c(colorBold), c(colorReset), nwsLoc, lat, lon)
	fmt.Printf("%sChecked:%s  %s\n\n",
		c(colorGray), c(colorReset), time.Now().Local().Format("Mon Jan 2, 3:04 PM MST"))

	// ── Active Alerts (always shown) ──
	fmt.Printf("%s━━━ Active Warnings & Advisories ━━━%s\n\n", c(colorBold), c(colorReset))

	alerts, err := getAlerts(lat, lon)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching alerts: %v\n", err)
		os.Exit(1)
	}

	if len(alerts) == 0 {
		fmt.Printf("%s✅ No active warnings or advisories.%s\n", c(colorGreen), c(colorReset))
	} else {
		fmt.Printf("%s%d active alert(s) found:%s\n\n", c(colorBold), len(alerts), c(colorReset))
		for i, f := range alerts {
			printAlert(f.Properties, *verbose, c)
			if i < len(alerts)-1 {
				fmt.Println()
				divider(c)
				fmt.Println()
			}
		}
	}

	// ── Current Conditions (shown when -daily or -weekly is requested) ──
	// We need the observation station list URL from the NWS points response.
	if *daily && pointsErr == nil && points.ObservationStations != "" {
		stationID, stationName, stErr := getNearestStation(points.ObservationStations)
		if stErr != nil {
			// Non-fatal: skip current conditions if the station list is unavailable.
			fmt.Fprintf(os.Stderr, "\nWarning: could not get observation station: %v\n", stErr)
		} else {
			obs, obsErr := getCurrentObservation(stationID)
			if obsErr != nil {
				fmt.Fprintf(os.Stderr, "\nWarning: could not fetch current conditions: %v\n", obsErr)
			} else {
				fmt.Println()
				divider(c)
				fmt.Println()
				fmt.Printf("%s━━━ Current Conditions ━━━%s\n\n", c(colorBold), c(colorReset))
				printCurrentConditions(obs, stationName, c)
			}
		}
	}

	// ── Daily forecast ────────────────────────────────────────────────────────
	if *daily && !*weekly {
		if pointsErr != nil || points.Forecast == "" {
			fmt.Fprintf(os.Stderr, "\nCould not retrieve NWS forecast grid: %v\n", pointsErr)
			os.Exit(1)
		}

		forecast, err := getForecast(points.Forecast)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nError fetching forecast: %v\n", err)
			os.Exit(1)
		}

		fmt.Println()
		divider(c)
		fmt.Println()
		fmt.Printf("%s━━━ Today's Forecast ━━━%s\n\n", c(colorBold), c(colorReset))

		// Show the first two periods: today's daytime and tonight.
		limit := 2
		if len(forecast.Periods) < limit {
			limit = len(forecast.Periods)
		}
		for i := 0; i < limit; i++ {
			printForecastPeriod(forecast.Periods[i], *verbose, c)
		}

		fmt.Printf("\n%sForecast updated: %s%s\n",
			c(colorGray), forecast.Updated.Local().Format("Mon Jan 2, 3:04 PM MST"), c(colorReset))
	}

	// ── Weekly forecast ───────────────────────────────────────────────────────
	if *weekly {
		if pointsErr != nil || points.Forecast == "" {
			fmt.Fprintf(os.Stderr, "\nCould not retrieve NWS forecast grid: %v\n", pointsErr)
			os.Exit(1)
		}

		forecast, err := getForecast(points.Forecast)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nError fetching forecast: %v\n", err)
			os.Exit(1)
		}

		fmt.Println()
		divider(c)
		fmt.Println()
		fmt.Printf("%s━━━ 7-Day Forecast ━━━%s\n\n", c(colorBold), c(colorReset))

		// Group consecutive periods into day/night pairs so we can print
		// each calendar day as a single logical block.
		type dayPair struct {
			day   *ForecastPeriod
			night *ForecastPeriod
		}
		var days []dayPair

		i := 0
		periods := forecast.Periods
		for i < len(periods) {
			p := periods[i]
			pair := dayPair{}
			if p.IsDaytime {
				pair.day = &periods[i]
				// Pair with the following night period if it exists.
				if i+1 < len(periods) && !periods[i+1].IsDaytime {
					pair.night = &periods[i+1]
					i += 2
				} else {
					i++
				}
			} else {
				// The forecast started mid-day (e.g. first period is "Tonight").
				pair.night = &periods[i]
				i++
			}
			days = append(days, pair)
		}

		// Cap at 7 calendar days.
		if len(days) > 7 {
			days = days[:7]
		}

		for di, pair := range days {
			// Print a date header above each day's periods.
			var dayName, dayDate string
			if pair.day != nil {
				dayName = pair.day.StartTime.Local().Format("Monday")
				dayDate = pair.day.StartTime.Local().Format("Jan 2")
			} else if pair.night != nil {
				dayName = pair.night.StartTime.Local().Format("Monday")
				dayDate = pair.night.StartTime.Local().Format("Jan 2")
			}
			fmt.Printf("  %s%s (%s)%s\n", c(colorBold+colorWhite), dayName, dayDate, c(colorReset))

			if pair.day != nil {
				printForecastPeriod(*pair.day, *verbose, c)
			}
			if pair.night != nil {
				printForecastPeriod(*pair.night, *verbose, c)
			}
			if di < len(days)-1 {
				fmt.Println()
			}
		}

		fmt.Printf("\n%sForecast updated: %s%s\n",
			c(colorGray), forecast.Updated.Local().Format("Mon Jan 2, 3:04 PM MST"), c(colorReset))
	}

	// ── Footer ────────────────────────────────────────────────────────────────
	fmt.Printf("%sSource: National Weather Service (api.weather.gov)%s\n", c(colorGray), c(colorReset))
	if !*daily && !*weekly {
		fmt.Printf("%sTip: add -daily or -weekly for current conditions + forecasts, -v for full details.%s\n", c(colorGray), c(colorReset))
	}
}
