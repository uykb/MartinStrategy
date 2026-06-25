package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Exchange ExchangeConfig `mapstructure:"exchange"`
	Strategy StrategyConfig `mapstructure:"strategy"`
	API      ApiConfig      `mapstructure:"api"`
	Log      LogConfig      `mapstructure:"log"`
}

type ExchangeConfig struct {
	ApiKey     string `mapstructure:"api_key"`
	ApiSecret  string `mapstructure:"api_secret"`
	Symbol     string `mapstructure:"symbol"`
	UseTestnet bool   `mapstructure:"use_testnet"`
}

type StrategyConfig struct {
	MaxSafetyOrders int     `mapstructure:"max_safety_orders"`
	BaseRatio       float64 `mapstructure:"base_ratio"`
}

type ApiConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Port      int    `mapstructure:"port"`
	AuthToken string `mapstructure:"auth_token"`
}

type LogConfig struct {
	Level string `mapstructure:"level"`
}

func LoadConfig(path string) (*Config, error) {
	viper.SetConfigFile(path)
	viper.SetConfigType("yaml")

	// Environment variables
	viper.SetEnvPrefix("MARTIN")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
