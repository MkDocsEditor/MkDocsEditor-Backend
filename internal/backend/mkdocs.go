package backend

import (
	"os"

	"gopkg.in/yaml.v3"
)

// readExtraCSS returns the extra_css files of mkdocs.yml. Other keys are not decoded, so an
// unexpected form of one of them (theme can be a plain string) does not matter.
func readExtraCSS(configFile string) ([]string, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	var config struct {
		ExtraCSS []string `yaml:"extra_css"`
	}
	if err = yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return config.ExtraCSS, nil
}
