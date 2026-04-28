package ipconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type Info struct {
	IP         string `json:"ip"`
	Country    string `json:"country"`
	CountryISO string `json:"country_iso"`
	City       string `json:"city"`
	TimeZone   string `json:"time_zone"`
}

type Client struct {
	logger *slog.Logger
}

func (c *Client) GetIPInfo(ctx context.Context, ip string) (domain.IpInfo, error) {
	const op = "adapters.ipconfig.GetIPInfo"

	req, err := http.NewRequestWithContext(ctx, "GET", "https://ifconfig.co/json?ip="+ip, nil)
	if err != nil {
		c.logger.Error("failed to make request ipconfig api")
		return domain.IpInfo{}, fmt.Errorf("%s:%w", op, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.logger.Error("failed to execute request ipconfig api")
		return domain.IpInfo{}, fmt.Errorf("%s:%w", op, err)

	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("unexpected response status", "status", resp.Status)
		return domain.IpInfo{}, fmt.Errorf("%s:failed to get IP config: %s", op, resp.Status)
	}

	var config Info
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		c.logger.Error("failed to unmarshall")
		return domain.IpInfo{}, fmt.Errorf("%s:%w", op, err)
	}

	return domain.IpInfo{
		Country:    config.Country,
		CountryISO: config.CountryISO,
		City:       config.City,
		TimeZone:   config.TimeZone,
		IP:         config.IP,
	}, nil
}

func New(logger *slog.Logger) *Client {
	return &Client{
		logger: logger,
	}
}
