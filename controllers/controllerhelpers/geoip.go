package controllerhelpers

import (
	"net"
	"strings"

	"github.com/TF2Stadium/Helen/config"
	"github.com/TF2Stadium/Helen/helpers"
	"github.com/oschwald/geoip2-golang"
)

var geodb *geoip2.Reader

func InitDB() {
	if config.Constants.GeoIP == "" {
		return
	}

	var err error
	geodb, err = geoip2.Open(config.Constants.GeoIP)

	if err != nil {
		helpers.Logger.Fatal(err.Error())
	}
}

func GetRegion(server string) string {
	if config.Constants.GeoIP == "" {
		return ""
	}

	arr := strings.Split(server, ":")
	ip := net.ParseIP(arr[0])
	record, err := geodb.Country(ip)
	if err != nil {
		helpers.Logger.Error(err.Error())
		return ""
	}
	return record.Continent.Names["en"]
}
