package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Exchange ExchangeConfig `mapstructure:"exchange"`
	Strategy StrategyConfig `mapstructure:"strategy"`
	Storage  StorageConfig  `mapstructure:"storage"`
	Log      LogConfig      `mapstructure:"log"`
}

type ExchangeConfig struct {
	ApiKey    string `mapstructure:"api_key"`
	ApiSecret string `mapstructure:"api_secret"`
	Symbol    string `mapstructure:"symbol"`
	UseTestnet bool   `mapstructure:"use_testnet"`
}

type StrategyConfig struct {
	BaseOrderSize           float64 `mapstructure:"base_order_size"`
	SafetyOrderSize         float64 `mapstructure:"safety_order_size"`
	MaxSafetyOrders         int     `mapstructure:"max_safety_orders"`
	VolumeScale             float64 `mapstructure:"volume_scale"` // Martingale multiplier
	SafetyOrderStepScale    float64 `mapstructure:"step_scale"`   // Grid step multiplier
	TargetProfit            float64 `mapstructure:"target_profit"`
	AtrPeriod               int     `mapstructure:"atr_period"`
	AtrMultiplier           float64 `mapstructure:"atr_multiplier"` // Grid spacing = ATR * Multiplier
}

type StorageConfig struct {
	SqlitePath string `mapstructure:"sqlite_path"`
	RedisAddr  string `mapstructure:"redis_addr"`
	RedisPass  string `mapstructure:"redis_pass"`
	RedisDB    int    `mapstructure:"redis_db"`
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
