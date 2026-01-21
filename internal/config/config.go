// Package config provides configuration loading from environment variables.
package config

import (
	"github.com/caarlos0/env/v10"
)

type Config struct {
	BindAddr string `env:"MITHLOND_BIND_ADDR" envDefault:"0.0.0.0:23123"`
	APIKey   string `env:"MITHLOND_API_KEY"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
