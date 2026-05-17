package config

import (
	"context"
	"encoding/json"

	"fmt"
	"log"
	"os"
	"time"
)

// Config holds all configuration parameters
type Config struct {
	DynamicConfig bool             `json:"enable_dynamic_config,omitempty"` // If config file gets updated, reload the config.
	Exchanges     []ExchangeConfig `json:"exchanges"`                       // Required
	Model         ModelConfig      `json:"model,omitempty"`
	Dashboard     DashboardConfig  `json:"dashboard,omitempty"` // Dashboard HTML / UX (optional)
}

// ExchangeConfig holds exchange-specific configuration
type ExchangeConfig struct {
	Name        string `json:"name"`                 // Exchange name (e.g., "hyperliquid", "lighter")
	Enabled     bool   `json:"enabled"`              // Enable or disable the exchange
	Testnet     bool   `json:"testnet,omitempty"`    // Use testnet or mainnet
	BaseURL     string `json:"base_url,omitempty"`   // Optional exchange REST endpoint override
	APIKey      string `json:"api_key,omitempty"`    // Exchange API key or account address, depending on venue
	APISecret   string `json:"api_secret,omitempty"` // Exchange API secret/private key
	APIKeyIndex uint8  `json:"api_key_index,omitempty"`
	Debug       bool   `json:"debug,omitempty"` // Enable debug logging for orders and positions
}

// DashboardConfig configures the HTTP dashboard (all fields optional).
type DashboardConfig struct {
	ChartWindow     string  `json:"chart_window,omitempty"`
	RefreshInterval string  `json:"refresh_interval,omitempty"`
	ChartScaleRatio float64 `json:"chart_scale_ratio,omitempty"`
}

// ModelConfig configures the market model (optional).
type ModelConfig struct {
	Pairs      []string                  `json:"pairs,omitempty"`
	Trailguard *TrailguardStrategyConfig `json:"trailguard,omitempty"`
}

// LoadConfig loads configuration from a file
func LoadConfig(path string, ctx context.Context) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	// Start config reloader goroutine if dynamic config is enabled
	if cfg.DynamicConfig {
		log.Println("Dynamic config is enabled (reloads when file changes)")
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()

			var lastMod time.Time
			if stat, err := os.Stat(path); err != nil {
				log.Printf("Failed to stat config file: %v\n", err)
			} else {
				lastMod = stat.ModTime()
			}

			for {
				select {
				case <-ticker.C:
					stat, err := os.Stat(path)
					if err != nil {
						log.Printf("Failed to stat config file: %v\n", err)
						continue
					}
					if stat.ModTime().Equal(lastMod) {
						continue
					}
					newData, err := os.ReadFile(path)
					if err != nil {
						log.Printf("Failed to read config file: %v\n", err)
						continue
					}
					if err := json.Unmarshal(newData, cfg); err != nil {
						log.Printf("Failed to parse config file: %v\n", err)
						continue
					}
					lastMod = stat.ModTime()
					log.Println("Config reloaded successfully")
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return cfg, nil
}

// PrintConfig logs model-related settings from config.
func PrintConfig(m *ModelConfig) {
	if m != nil && len(m.Pairs) > 0 {
		log.Printf("Model pair filter: %v", m.Pairs)
		return
	}
	log.Println("Model pair filter: (none — subscribe all enabled perpetual pairs)")
}

func (sc *ModelConfig) Print() {
	log.Println()
	log.Printf("MODEL CONFIGURATION:")
	log.Println("Trader Settings:")
	log.Printf("  Pairs                : %v", sc.Pairs)
	if sc.Trailguard != nil && sc.Trailguard.Enabled {
		log.Printf("  Trailguard           : enabled mode=%s", sc.Trailguard.Mode)
	} else {
		log.Printf("  Trailguard           : (disabled)")
	}
	log.Println()
}
