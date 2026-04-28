package domain

import "time"

const TspRegistrationPrefix = "tsp-"

type Provider struct {
	PublicKey    string    `json:"public_key"`
	Address      string    `json:"address"`
	LT           uint64    `json:"lt"`
	RegisteredAt time.Time `json:"registered_at"`
}

type Rates struct {
	RatePerMBDay int64  `json:"rate_per_mb_day"`
	MinBounty    int64  `json:"min_bounty"`
	MinSpan      uint32 `json:"min_span"`
	MaxSpan      uint32 `json:"max_span"`
}

type Endpoint struct {
	PublicKey []byte `json:"public_key"`
	IP        string `json:"ip"`
	Port      int32  `json:"port"`
}

type ProviderEndpoint struct {
	PublicKey string    `json:"public_key"`
	Provider  Endpoint  `json:"provider"`
	Storage   Endpoint  `json:"storage"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ProviderStatus struct {
	PublicKey string    `json:"public_key"`
	IsOnline  bool      `json:"is_online"`
	CheckedAt time.Time `json:"checked_at"`
}

type IpInfo struct {
	Country    string `json:"country"`
	CountryISO string `json:"country_iso"`
	City       string `json:"city"`
	TimeZone   string `json:"timezone"`
	IP         string `json:"ip"`
}
