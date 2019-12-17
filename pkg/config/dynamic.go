package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var defaults map[string]string = map[string]string{
	"maxSurge":              "1",
	"maxUnavailable":        "0",
	"deleteOldLaunchConfig": "false",
	"deletionAge":           "",
	"deletionAgeJitter":     "",
	"startupGracePeriod":    "",
	"ignoreSelector":        "kubernetes.io/role=master",
	"ignore":                "false",
}

// DynamicConfig represents the settings specified by configmap
type DynamicConfig struct {
	settings map[string]map[string]string
}

// Reload loads the settings from the mounted configmap
func (c *DynamicConfig) Reload() error {
	logrus.Trace("Reloading configmap...")
	get := map[string]string{}

	if _, err := os.Stat("/etc/config"); os.IsNotExist(err) {
		logrus.Tracef("/etc/config does not exist. Skipping config load")
		return nil
	}

	files, err := ioutil.ReadDir("/etc/config")
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
			continue
		}
		contents, err := ioutil.ReadFile("/etc/config/" + file.Name())
		if err != nil {
			return fmt.Errorf("Error reading %v: %v", file.Name(), err)
		}
		get[file.Name()] = string(contents)

	}
	c.loadFromMap(get)
	return nil
}

func (c *DynamicConfig) loadFromMap(inp map[string]string) {
	newSettings := map[string]map[string]string{}
	for key, value := range inp {
		keyParts := strings.Split(key, ".")
		if len(keyParts) < 2 {
			logrus.Warnf("Ignoring key %v", key)
			continue
		}
		if keyParts[0] == "global" {
			if len(keyParts) != 2 {
				logrus.Warnf("Ignoring key %v", key)
				continue
			}
			if _, ok := newSettings[""]; !ok {
				newSettings[""] = map[string]string{}
			}
			newSettings[""][keyParts[1]] = value
		} else if keyParts[0] == "group" {
			if len(keyParts) < 3 {
				logrus.Warnf("Ignoring key %v", key)
				continue
			}

			group := strings.Join(keyParts[1:len(keyParts)-1], ".")
			if _, ok := newSettings[group]; !ok {
				newSettings[group] = map[string]string{}
			}
			newSettings[group][keyParts[len(keyParts)-1]] = value
		}
	}

	c.settings = newSettings
}

// GetString returns a string from a configmap
func (c *DynamicConfig) GetString(groupName, key string) string {
	if groupSettings, ok := c.settings[groupName]; ok {
		if setting, ok := groupSettings[key]; ok {
			return setting
		}
	}
	if globalSettings, ok := c.settings[""]; ok {
		if setting, ok := globalSettings[key]; ok {
			return setting
		}
	}
	if defaultSetting, ok := defaults[key]; ok {
		return defaultSetting
	}

	panic("No default exists for setting " + key)
}

// GetBool returns a bool parsed from a configmap key
func (c *DynamicConfig) GetBool(groupName, key string) bool {
	if groupSettings, ok := c.settings[groupName]; ok {
		if setting, ok := groupSettings[key]; ok {
			return parseBool(setting)
		}
	}
	if globalSettings, ok := c.settings[""]; ok {
		if setting, ok := globalSettings[key]; ok {
			return parseBool(setting)
		}
	}
	if defaultSetting, ok := defaults[key]; ok {
		return parseBool(defaultSetting)
	}
	panic("No default exists for setting " + key)
}

// GetDuration returns a time.Duration parsed from a configmap key
func (c *DynamicConfig) GetDuration(groupName, key string) *time.Duration {
	if groupSettings, ok := c.settings[groupName]; ok {
		if setting, ok := groupSettings[key]; ok {
			return parseDuration(setting)
		}
	}
	if globalSettings, ok := c.settings[""]; ok {
		if setting, ok := globalSettings[key]; ok {
			return parseDuration(setting)
		}
	}
	if defaultSetting, ok := defaults[key]; ok {
		return parseDuration(defaultSetting)
	}
	panic("No default exists for setting " + key)
}

func parseBool(s string) bool {
	if s == "true" {
		return true
	} else if s == "false" {
		return false
	}
	panic("Boolean value '" + s + "' is neither 'true' nor 'false'")
}

func parseDuration(s string) *time.Duration {
	if s == "" {
		return nil
	}
	d, err := ParseDuration(s)
	if err != nil {
		panic(fmt.Sprintf("Duration %v is not valid: %v", s, err))
	}
	return &d
}
