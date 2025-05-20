package internal

import (
	"log"

	"github.com/spf13/viper"
)

type Service struct {
	DefaultPort int    `mapstructure:"defaultPort"`
	LocalPort   int    `mapstructure:"localPort"`
	Endpoint    string `mapstructure:"endpoint"`
}

type Site struct {
	KubeContext string             `mapstructure:"kubeContext"`
	Porxy       string             `mapstructure:"proxy"`
	Namespace   string             `mapstructure:"namespace"`
	Services    map[string]Service `mapstructure:"services"`
}

type Config struct {
	Sites map[string]Site `mapstructure:"sites"`
}

var Cfg Config

func Init(configPath string) {
	viper.SetConfigFile(configPath)
	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}
	if err := viper.Unmarshal(&Cfg); err != nil {
		log.Fatalf("Error parsing config: %v", err)
	}
}
