# wxwarn — NWS Weather Warnings CLI

A zero-dependency Go tool that scrapes the National Weather Service API
for active weather warnings, watches, advisories, **current conditions**, and forecasts.

## Build

```bash
go build -o wxwarn .
# or install globally:
go install .
```

## Usage

```bash
# Default: Bozeman, MT — active warnings only
./wxwarn

# Switch location via flag
./wxwarn -loc "Great Falls, MT"
./wxwarn -loc "Missoula, MT"
./wxwarn -loc "Billings, MT"
./wxwarn -loc "59715"              # zip code works too

# Positional arg (no flag needed)
./wxwarn "Kalispell, MT"

# Current conditions + today's forecast (warnings + current obs + daytime + tonight)
./wxwarn -daily
./wxwarn -daily -loc "Helena, MT"

# Current conditions + full 7-day forecast (warnings + current obs + all 7 days)
./wxwarn -weekly
./wxwarn -weekly -loc "Missoula, MT"

# Verbose: full alert text + detailed forecast narrative
./wxwarn -v -weekly

# No color (for piping / logging / cron)
./wxwarn -no-color -loc "Butte, MT"
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-loc` | `Bozeman, MT` | City/state or zip code |
| `-daily` | false | Show current conditions + today's forecast (daytime + tonight) |
| `-weekly` | false | Show current conditions + full 7-day forecast |
| `-v` | false | Show full alert text, detailed forecast narrative |
| `-no-color` | false | Disable ANSI color output |

> `-weekly` includes everything `-daily` shows plus the rest of the week.

## Output Sections

When `-daily` or `-weekly` is used, output is displayed in this order:

1. **Active Warnings & Advisories** — always shown; NWS alerts for your location
2. **Current Conditions** — live METAR observation from the nearest airport/station:
   - Sky / description
   - Temperature (with wind chill or heat index when applicable)
   - Dewpoint and relative humidity
   - Wind speed, direction, and gusts
   - Visibility
   - Barometric pressure
3. **Today's Forecast** (`-daily`) or **7-Day Forecast** (`-weekly`)

## Data Sources

- **Geocoding**: OpenStreetMap Nominatim (no API key needed)
- **Weather alerts**: National Weather Service `api.weather.gov` (no API key needed)
- **Current conditions**: NWS METAR observations via `api.weather.gov/stations/{ID}/observations/latest`
- **Forecast**: NWS gridded forecast via `api.weather.gov`

## Alert Severity Levels

| Icon | Severity | Meaning |
|------|----------|---------|
| 🚨 | Extreme | Extraordinary threat to life or property |
| ⚠️  | Severe | Significant threat |
| 🔶 | Moderate | Possible threat |
| 🔵 | Minor | Minimal threat |

## Tip: Make it a shell alias

Add to your `~/.bashrc` or `~/.zshrc`:

```bash
alias wxwarn='~/bin/wxwarn -loc "Bozeman, MT"'
alias wxbzn='~/bin/wxwarn -loc "Bozeman, MT" -v'
alias wxgf='~/bin/wxwarn -loc "Great Falls, MT"'
```

## Cron / scheduled checks

```bash
# Check every hour, log to file
0 * * * * /usr/local/bin/wxwarn -no-color -daily -loc "Bozeman, MT" >> /var/log/wxwarn.log 2>&1
```
