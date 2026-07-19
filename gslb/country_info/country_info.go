// https://github.com/eesur/country-codes-lat-long/blob/master/country-codes-lat-long-alpha3.json

package countryinfo

import (
	"encoding/json"
	"os"
	"strings"
)

type CountryInfo struct {
	RefCountryCodes []Country `json:"ref_country_codes"`
}

type Country struct {
	Alpha2    string  `json:"alpha2"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type LatLon struct {
	Lat float64
	Lon float64
}

func MakeMap(path string) (map[string]LatLon, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var info CountryInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	ccmap := make(map[string]LatLon)
	for _, latlong := range info.RefCountryCodes {
		ccmap[strings.ToLower(latlong.Alpha2)] = LatLon{Lat: latlong.Latitude, Lon: latlong.Longitude}
	}
	return ccmap, nil
}
