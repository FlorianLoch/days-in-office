package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geo"
)

func main() {
	inputDirFlag := flag.String("input-dir", "", "Directory containing the input JSON files")
	startDateFlag := flag.String("start-date", "", "Start of time range to consider, example: 2020-01-01T00:00:00")
	endDateFlag := flag.String("end-date", "", "End of time range to consider")
	latitudeFlag := flag.String("latitude", "", "Latitude of the location")
	longitudeFlag := flag.String("longitude", "", "Longitude of the location")
	toleranceFlag := flag.String("tolerance", "1000", "Radius around location in meters, contained places are considered as the location ")
	verboseFlag := flag.Bool("verbose", false, "Verbose output")
	printDatesFlag := flag.Bool("print-dates", false, "Print dates")

	flag.Parse()

	logLevel := log.InfoLevel
	if *verboseFlag {
		logLevel = log.DebugLevel
	}

	log.SetDefault(log.NewWithOptions(os.Stderr, log.Options{
		Level: logLevel,
	}))

	// TODO: Validate input

	latitude, err := strconv.ParseFloat(*latitudeFlag, 64)
	if err != nil {
		log.Error("Could not parse latitude", "err", err)
	}

	longitude, err := strconv.ParseFloat(*longitudeFlag, 64)
	if err != nil {
		log.Error("Could not parse longitude", "err", err)

	}

	tolerance, err := strconv.ParseFloat(*toleranceFlag, 64)
	if err != nil {
		log.Error("Could not parse tolerance", "err", err)
	}

	startDate, err := time.ParseInLocation(time.RFC3339, *startDateFlag, time.Local)
	if err != nil {
		log.Error("Could not parse start date", "err", err)
	}

	endDate, err := time.ParseInLocation(time.RFC3339, *endDateFlag, time.Local)
	if err != nil {
		log.Error("Could not parse end date", "err", err)
	}

	fileNames, err := listFilesRecursively(*inputDirFlag)
	if err != nil {
		log.Error("Could not list files", "err", err)
	}

	officeLocation := orb.Point{latitude, longitude}
	daysInTheOffice := make(dayMap)

	for _, fileName := range fileNames {
		processFile(fileName, startDate, endDate, officeLocation, tolerance, daysInTheOffice)
	}

	log.Infof("You have been in the office on %d day(s) of which %d have been working days.", len(daysInTheOffice), daysInTheOffice.CountWorkingDays())

	if *printDatesFlag {
		list := daysInTheOffice.ToSlice()

		sort.Strings(list)

		for _, date := range list {
			fmt.Print(date)

			if !daysInTheOffice[date] {
				fmt.Print(" (weekend)")
			}

			fmt.Print("\n")
		}
	}
}

// dayMap maps a stringified date to a boolean indicating whether it was a working day
type dayMap map[string]bool

func (d dayMap) Add(t time.Time) {
	date := t.Format("2006-01-02")
	d[date] = t.Weekday() != time.Saturday && t.Weekday() != time.Sunday
}

func (d dayMap) ToSlice() []string {
	slice := make([]string, 0, len(d))

	for key := range d {
		slice = append(slice, key)
	}

	return slice
}

func (d dayMap) CountWorkingDays() int {
	count := 0

	for _, isWorkingDay := range d {
		if isWorkingDay {
			count++
		}
	}

	return count
}

func processFile(fileName string, startDate, endDate time.Time, officeLocation orb.Point, tolerance float64, daysInTheOffice dayMap) {
	logger := log.With("file", fileName)

	file, err := os.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		logger.Error("Could not open file", "err", err)
	}

	places, err := ParseTimelineInput(file)
	if err != nil {
		logger.Error("Could not parse file", "err", err)
	}

	placesProcessed := 0

	for _, place := range places {
		if place.End.Before(startDate) || place.Start.After(endDate) {
			// We expect entries to be in sorted order, so we could break here.
			// But as we do not know for sure we instead go the extra mile.
			continue
		}

		loc := orb.Point{place.Latitude, place.Longitude}

		distance := geo.DistanceHaversine(officeLocation, loc)

		if distance <= tolerance {
			daysInTheOffice.Add(place.Start)
		}

		placesProcessed++
	}

	logger.Debugf("Found %d visits to places in file of which %d have been (partially) within the given time range", len(places), placesProcessed)
}

func listFilesRecursively(inputDir string) ([]string, error) {
	var list []string

	var readDir func(string) error
	readDir = func(inputDir string) error {
		entries, err := os.ReadDir(inputDir)
		if err != nil {
			return fmt.Errorf("could not read directory %s: %w", inputDir, err)
		}

		for _, entry := range entries {
			fullPath := path.Join(inputDir, entry.Name())

			if entry.IsDir() {
				err := readDir(fullPath)
				if err != nil {
					return err
				}
			} else {
				list = append(list, fullPath)
			}
		}

		return nil
	}

	if err := readDir(inputDir); err != nil {
		return nil, err
	}

	return list, nil
}

func ParseTimelineInput(input io.Reader) ([]timelinePoint, error) {
	type wrapper struct {
		TimelineObjects []struct {
			PlaceVisit *timelineVisitedPlace `json:"placeVisit"`
		} `json:"timelineObjects"`

		SemanticSegments []semanticSegment `json:"semanticSegments"`
	}

	var w wrapper

	if err := json.NewDecoder(input).Decode(&w); err != nil {
		return nil, fmt.Errorf("decoding JSON: %w", err)
	}

	var result []timelinePoint

	// Check for the newer semantic location history format exported from local device
	if w.SemanticSegments != nil {
		for _, entry := range w.SemanticSegments {
			for _, point := range entry.TimelinePath {
				// Parse the point
				lat, long := parsePoint(point.Point)

				result = append(result, timelinePoint{
					Latitude:  lat,
					Longitude: long,
					Start:     entry.StartTime,
					End:       entry.EndTime,
				})
			}
		}

		return result, nil
	}

	// Remove nil entries, i.e. entries that are not place visits but activity segments or something else
	for _, entry := range w.TimelineObjects {
		if entry.PlaceVisit == nil {
			continue
		}

		// Google removed these two fields at some point, so we simply take the second best option.
		// See below.
		if entry.PlaceVisit.CenterLatE7 == 0 || entry.PlaceVisit.CenterLngE7 == 0 {
			entry.PlaceVisit.CenterLatE7 = entry.PlaceVisit.Location.LatitudeE7
			entry.PlaceVisit.CenterLngE7 = entry.PlaceVisit.Location.LongitudeE7
		}

		place := entry.PlaceVisit

		result = append(result, timelinePoint{
			Latitude:  float64(place.CenterLatE7) / 1e7,
			Longitude: float64(place.CenterLngE7) / 1e7,
			Start:     place.Duration.Start,
			End:       place.Duration.End,
		})
	}

	return result, nil
}

func parsePoint(value string) (float64, float64) {
	// "51.6503959°, 5.0492413°"
	coords := strings.Split(strings.ReplaceAll(value, "°", ""), ", ")

	lat, _ := strconv.ParseFloat(coords[0], 64)
	long, _ := strconv.ParseFloat(coords[1], 64)

	return lat, long
}

type timelinePoint struct {
	Latitude  float64
	Longitude float64

	Start time.Time
	End   time.Time
}

type timelineVisitedPlace struct {
	Location struct {
		LatitudeE7  int    `json:"latitudeE7"`
		LongitudeE7 int    `json:"longitudeE7"`
		Address     string `json:"address"`
		Name        string `json:"name"`
	} `json:"location"`
	Duration struct {
		Start time.Time `json:"startTimestamp"`
		End   time.Time `json:"endTimestamp"`
	} `json:"duration"`
	VisitConfidence int `json:"visitConfidence"`
	// It seems like Google removed these two fields on the 7th of February 2024 as they don't show up in records
	// after this date.
	CenterLatE7 int `json:"centerLatE7"`
	CenterLngE7 int `json:"centerLngE7"`
}

type semanticSegment struct {
	StartTime    time.Time `json:"startTime"`
	EndTime      time.Time `json:"endTime"`
	TimelinePath []struct {
		Point string    `json:"point"`
		Time  time.Time `json:"time"`
	} `json:"timelinePath"`
}
