package domain

import "time"

type Provider struct {
	PublicKey    string
	Address      string
	LT           uint64
	RegisteredAt time.Time
}

type Rates struct {
	RatePerMBDay int64
	MinBounty    int64
	MinSpan      uint32
	MaxSpan      uint32
}

type Endpoint struct {
	Publickey []byte
	IP        string
	Port      int32
}

type ProviderEndpoint struct {
	PublicKey string
	Provider  Endpoint
	Storage   Endpoint
	UpdatedAt time.Time
}

type ProviderStatus struct {
	PublicKey string
	IsOnline  bool
	CheckedAt time.Time
}

type IpInfo struct {
	Country    string
	CountryISO string
	City       string
	TimeZone   string
	ISP        string
}
