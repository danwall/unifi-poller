package unifipoller

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	influx "github.com/influxdata/influxdb1-client/v2"
	"github.com/spf13/pflag"
	"golift.io/unifi"
	yaml "gopkg.in/yaml.v2"
)

// Version is injected by the Makefile
var Version = "development"

const (
	// App defaults in case they're missing from the config.
	defaultInterval   = 30 * time.Second
	defaultInfluxDB   = "unifi"
	defaultInfluxUser = "unifi"
	defaultInfluxPass = "unifi"
	defaultInfluxURL  = "http://127.0.0.1:8086"
	defaultUnifiUser  = "influx"
	defaultUnifiURL   = "https://127.0.0.1:8443"
)

// ENVConfigPrefix is the prefix appended to an env variable tag
// name before retrieving the value from the OS.
const ENVConfigPrefix = "UP_"

// UnifiPoller contains the application startup data, and auth info for UniFi & Influx.
type UnifiPoller struct {
	Influx     influx.Client
	Unifi      *unifi.Unifi
	Flag       *Flag
	Config     *Config
	errorCount int
	LastCheck  time.Time
}

// Flag represents the CLI args available and their settings.
type Flag struct {
	ConfigFile string
	DumpJSON   string
	ShowVer    bool
	*pflag.FlagSet
}

// Metrics contains all the data from the controller and an influx endpoint to send it to.
type Metrics struct {
	TS time.Time
	unifi.Sites
	unifi.IDSList
	unifi.Clients
	*unifi.Devices
	influx.BatchPoints
}

// Config represents the data needed to poll a controller and report to influxdb.
// This is all of the data stored in the config file.
// Any with explicit defaults have _omitempty on json and toml tags.
type Config struct {
	MaxErrors  int      `json:"max_errors" toml:"max_errors" xml:"max_errors" yaml:"max_errors" env:"MAX_ERRORS"`
	Interval   Duration `json:"interval,_omitempty" toml:"interval,_omitempty" xml:"interval" yaml:"interval" env:"POLLING_INTERVAL"`
	Debug      bool     `json:"debug" toml:"debug" xml:"debug" yaml:"debug" env:"DEBUG_MODE"`
	Quiet      bool     `json:"quiet,_omitempty" toml:"quiet,_omitempty" xml:"quiet" yaml:"quiet" env:"QUIET_MODE"`
	VerifySSL  bool     `json:"verify_ssl" toml:"verify_ssl" xml:"verify_ssl" yaml:"verify_ssl" env:"VERIFY_SSL"`
	CollectIDS bool     `json:"collect_ids" toml:"collect_ids" xml:"collect_ids" yaml:"collect_ids" env:"COLLECT_IDS"`
	ReAuth     bool     `json:"reauthenticate" toml:"reauthenticate" xml:"reauthenticate" yaml:"reauthenticate" env:"REAUTHENTICATE"`
	Mode       string   `json:"mode" toml:"mode" xml:"mode" yaml:"mode" env:"POLLING_MODE"`
	InfluxURL  string   `json:"influx_url,_omitempty" toml:"influx_url,_omitempty" xml:"influx_url" yaml:"influx_url" env:"INFLUX_URL"`
	InfluxUser string   `json:"influx_user,_omitempty" toml:"influx_user,_omitempty" xml:"influx_user" yaml:"influx_user" env:"INFLUX_USER"`
	InfluxPass string   `json:"influx_pass,_omitempty" toml:"influx_pass,_omitempty" xml:"influx_pass" yaml:"influx_pass" env:"INFLUX_PASS"`
	InfluxDB   string   `json:"influx_db,_omitempty" toml:"influx_db,_omitempty" xml:"influx_db" yaml:"influx_db" env:"INFLUX_DB"`
	UnifiUser  string   `json:"unifi_user,_omitempty" toml:"unifi_user,_omitempty" xml:"unifi_user" yaml:"unifi_user" env:"UNIFI_USER"`
	UnifiPass  string   `json:"unifi_pass,_omitempty" toml:"unifi_pass,_omitempty" xml:"unifi_pass" yaml:"unifi_pass" env:"UNIFI_PASS"`
	UnifiBase  string   `json:"unifi_url,_omitempty" toml:"unifi_url,_omitempty" xml:"unifi_url" yaml:"unifi_url" env:"UNIFI_URL"`
	Sites      []string `json:"sites,_omitempty" toml:"sites,_omitempty" xml:"sites" yaml:"sites" env:"POLL_SITES"`
}

// Duration is used to UnmarshalTOML into a time.Duration value.
type Duration struct{ time.Duration }

// UnmarshalText parses a duration type from a config file.
func (d *Duration) UnmarshalText(data []byte) (err error) {
	d.Duration, err = time.ParseDuration(string(data))
	return
}

// ParseFile parses and returns our configuration data.
func (c *Config) ParseFile(configFile string) error {
	switch buf, err := ioutil.ReadFile(configFile); {
	case err != nil:
		return err
	case strings.Contains(configFile, ".json"):
		return json.Unmarshal(buf, c)
	case strings.Contains(configFile, ".xml"):
		return xml.Unmarshal(buf, c)
	case strings.Contains(configFile, ".yaml"):
		return yaml.Unmarshal(buf, c)
	default:
		return toml.Unmarshal(buf, c)
	}
}

// ParseENV copies environment variables into configuration values.
// This is useful for Docker users that find it easier to pass ENV variables
// than a specific configuration file. Uses reflection to find struct tags.
func (c *Config) ParseENV() error {
	t := reflect.TypeOf(Config{}) // Get tag names from the Config struct.
	// Loop each Config struct member; get reflect tag & env var value; update config.
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("env")        // Get the ENV variable name from "env" struct tag
		env := os.Getenv(ENVConfigPrefix + tag) // Then pull value from OS.
		if tag == "" || env == "" {
			continue // Skip if either are empty.
		}

		// Reflect and update the u.Config struct member at position i.
		switch c := reflect.ValueOf(c).Elem().Field(i); c.Type().String() {
		// Handle each member type appropriately (differently).
		case "string":
			// This is a reflect package method to update a struct member by index.
			c.SetString(env)
		case "int":
			val, err := strconv.Atoi(env)
			if err != nil {
				return fmt.Errorf("%s: %v", tag, err)
			}
			c.Set(reflect.ValueOf(val))
		case "[]string":
			c.Set(reflect.ValueOf(strings.Split(env, ",")))
		case path.Base(t.PkgPath()) + ".Duration":
			val, err := time.ParseDuration(env)
			if err != nil {
				return fmt.Errorf("%s: %v", tag, err)
			}
			c.Set(reflect.ValueOf(Duration{val}))
		case "bool":
			val, err := strconv.ParseBool(env)
			if err != nil {
				return fmt.Errorf("%s: %v", tag, err)
			}
			c.SetBool(val)
		}
	}
	return nil
}
