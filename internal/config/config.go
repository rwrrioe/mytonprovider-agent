package config

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/ilyakaznacheev/cleanenv"
)

var LogLevels = map[uint8]slog.Level{
	0: slog.LevelDebug,
	1: slog.LevelInfo,
	2: slog.LevelWarn,
	3: slog.LevelError,
}

type System struct {
	AgentID       string             `yaml:"agent_id"  env:"AGENT_ID"  env-default:"auto"`
	ADNLPort      string             `yaml:"adnl_port" env:"ADNL_PORT" env-default:"16167"`
	Key           ed25519.PrivateKey `env:"SYSTEM_KEY"`
	LogLevel      uint8              `yaml:"log_level" env:"LOG_LEVEL" env-default:"1"`
	MinJobIdle    time.Duration      `yaml:"min_job_idle" env:"MIN_JOB_IDLE" env-default:"15m"`
	ReaperTimeout time.Duration      `yaml:"reaper_timeout" env:"REAPER_TIMEOUT" env-default:"10m"`
}

type Postgres struct {
	Host     string `yaml:"db_host"     env:"DB_HOST"     env-required:"true"`
	Port     string `yaml:"db_port"     env:"DB_PORT"     env-required:"true"`
	User     string `yaml:"db_user"     env:"DB_USER"     env-required:"true"`
	Password string `yaml:"db_password" env:"DB_PASSWORD" env-required:"true"`
	Name     string `yaml:"db_name"     env:"DB_NAME"     env-required:"true"`
}

type Redis struct {
	Addr         string `yaml:"addr"          env:"REDIS_ADDR"          env-default:"redis:6379"`
	Password     string `                     env:"REDIS_PASSWORD"     env-default:""`
	DB           int    `yaml:"db"            env:"REDIS_DB"            env-default:"0"`
	Group        string `yaml:"group"         env:"REDIS_GROUP"         env-default:"mtpa"`
	StreamPrefix string `yaml:"stream_prefix" env:"REDIS_STREAM_PREFIX" env-default:"mtpa"`
	ResultMaxLen int64  `yaml:"result_maxlen"                           env-default:"100000"`
}

type TON struct {
	MasterAddress string `env:"MASTER_ADDRESS"                   env-required:"true"`
	ConfigURL     string `yaml:"config_url" env:"TON_CONFIG_URL" env-required:"true"`
}

// UsecaseCfg общие настройки для всех циклов одного юзкейса
// pool применяется к каждому циклу отдельно (discovery pool=2 эжто 2 consumer
// в каждом из scan_master,scan_wallets,resolve_endpoints)
type UsecaseCfg struct {
	Enabled     bool          `yaml:"enabled"`
	Pool        int           `yaml:"pool"         env-default:"1"`
	Timeout     time.Duration `yaml:"timeout"      env-default:"30m"`
	BlockMs     int           `yaml:"block_ms"     env-default:"5000"`
	Concurrency int           `yaml:"concurrency"  env-default:"0"` // внутр. параллелизм цикла (probe/verify)
	EndpointTTL time.Duration `yaml:"endpoint_ttl" env-default:"30m"`
}

type Workers struct {
	Discovery UsecaseCfg `yaml:"discovery"`
	Poll      UsecaseCfg `yaml:"poll"`
	Proof     UsecaseCfg `yaml:"proof"`
	Update    UsecaseCfg `yaml:"update"`
}

type Metrics struct {
	Enabled bool   `yaml:"enabled" env:"METRICS_ENABLED" env-default:"true"`
	Port    string `yaml:"port"    env:"METRICS_PORT"    env-default:"2112"`
}

type Config struct {
	System   System   `yaml:"system"`
	Postgres Postgres `yaml:"postgres"`
	Redis    Redis    `yaml:"redis"`
	TON      TON      `yaml:"ton"`
	Workers  Workers  `yaml:"workers"`
	Metrics  Metrics  `yaml:"metrics"`
}

func MustLoad() *Config {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		panic("CONFIG_PATH is not set")
	}

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("config file %q does not exist", configPath))
	}

	var cfg Config
	if err := cleanenv.ReadConfig(configPath, &cfg); err != nil {
		panic("failed to read config: " + err.Error())
	}

	if cfg.System.AgentID == "" || cfg.System.AgentID == "auto" {
		cfg.System.AgentID = "agent-" + uuid.NewString()
	}

	if len(cfg.System.Key) == 0 {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			panic("failed to generate ed25519 key: " + err.Error())
		}
		cfg.System.Key = ed25519.NewKeyFromSeed(priv.Seed())
	}

	return &cfg
}

func (p Postgres) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		p.User, p.Password, p.Host, p.Port, p.Name,
	)
}
