package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds all configuration from environment variables
type Config struct {
	// Health check
	HealthURL        string        // URL to check via tunnel (e.g. https://health.octopus-review.ai/api/version)
	HealthHostHeader string        // Host header override (e.g. octopus-review.ai)
	CheckInterval    time.Duration // How often to check (default: 5s)
	FailThreshold    int           // Consecutive failures before failover (default: 3)
	RecoverThreshold int           // Consecutive successes before failback (default: 3)
	RequestTimeout   time.Duration // HTTP timeout per check (default: 10s)

	// Cloudflare
	CFAPIToken  string // Cloudflare API token with DNS edit + Tunnel read permissions
	CFAccountID string // Cloudflare account ID
	CFZoneID    string // Zone ID for the domain
	CFTunnelID  string // Tunnel ID to monitor
	Domain      string // Domain name (e.g. octopus-review.ai)

	// Origins
	PrimaryCNAME string // Tunnel CNAME (e.g. <tunnel-id>.cfargotunnel.com)
	FailoverIP   string // AWS static IP

	// Slack
	SlackWebhookURL string // Slack incoming webhook for #octopus-events

	// Twilio
	TwilioAccountSID string   // Twilio Account SID
	TwilioAuthToken  string   // Twilio Auth Token
	TwilioFromNumber string   // Twilio phone number (caller)
	TwilioToNumbers  []string // Phone numbers to call in order

	// State
	DryRun bool // If true, log but don't actually change DNS
}

type State struct {
	CurrentOrigin    string // "primary" or "failover"
	ConsecutiveFails int
	ConsecutiveOKs   int
	LastCheck        time.Time
	LastSwitch       time.Time
	LastSlackEvent   string    // "failover" or "failback" - prevents duplicate notifications
	LastSlackTime    time.Time // cooldown: don't spam Slack during flapping
	LastTwilioCall   time.Time // cooldown: don't call again within 30 minutes
	TwilioCallPending bool    // true if a delayed call is already scheduled
}

// Cloudflare API types
type CFDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type CFResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

type CFTunnelStatus struct {
	Status      string `json:"status"`
	Connections []struct {
		ColoName string `json:"colo_name"`
	} `json:"connections"`
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("CHECK_INTERVAL", "5s"))
	timeout, _ := time.ParseDuration(getEnv("REQUEST_TIMEOUT", "10s"))

	tunnelID := getEnv("CF_TUNNEL_ID", "")
	primaryCNAME := getEnv("PRIMARY_CNAME", "")

	// Derive tunnel ID from CNAME if not set
	if tunnelID == "" && primaryCNAME != "" {
		tunnelID = strings.TrimSuffix(primaryCNAME, ".cfargotunnel.com")
	}

	domain := getEnv("DOMAIN", "octopus-review.ai")

	return Config{
		HealthURL:        getEnv("HEALTH_URL", "https://health."+domain+"/api/version"),
		HealthHostHeader: getEnv("HEALTH_HOST_HEADER", domain),
		CheckInterval:    interval,
		FailThreshold:    getEnvInt("FAIL_THRESHOLD", 3),
		RecoverThreshold: getEnvInt("RECOVER_THRESHOLD", 3),
		RequestTimeout:   timeout,
		CFAPIToken:       mustGetEnv("CF_API_TOKEN"),
		CFAccountID:      mustGetEnv("CF_ACCOUNT_ID"),
		CFZoneID:         mustGetEnv("CF_ZONE_ID"),
		CFTunnelID:       tunnelID,
		Domain:           domain,
		PrimaryCNAME:     primaryCNAME,
		FailoverIP:       mustGetEnv("FAILOVER_IP"),
		SlackWebhookURL:  getEnv("SLACK_WEBHOOK_URL", ""),
		TwilioAccountSID: getEnv("TWILIO_ACCOUNT_SID", ""),
		TwilioAuthToken:  getEnv("TWILIO_AUTH_TOKEN", ""),
		TwilioFromNumber: getEnv("TWILIO_FROM_NUMBER", ""),
		TwilioToNumbers:  parseTwilioNumbers(getEnv("TWILIO_TO_NUMBER", "")),
		DryRun:           getEnv("DRY_RUN", "false") == "true",
	}
}

func main() {
	cfg := loadConfig()

	log.Printf("Starting failover monitor for %s", cfg.Domain)
	log.Printf("Health check: %s (Host: %s)", cfg.HealthURL, cfg.HealthHostHeader)
	log.Printf("Tunnel ID: %s", cfg.CFTunnelID)
	log.Printf("Check interval: %s, Fail threshold: %d, Recover threshold: %d",
		cfg.CheckInterval, cfg.FailThreshold, cfg.RecoverThreshold)
	log.Printf("Primary CNAME: %s, Failover IP: %s", cfg.PrimaryCNAME, cfg.FailoverIP)
	log.Printf("Dry run: %v", cfg.DryRun)

	state := &State{
		CurrentOrigin: "primary",
		LastCheck:     time.Now(),
	}

	// Determine initial state from current DNS
	currentOrigin := detectCurrentOrigin(cfg)
	if currentOrigin != "" {
		state.CurrentOrigin = currentOrigin
		if currentOrigin == "failover" {
			state.LastSlackEvent = "failover"
		} else {
			state.LastSlackEvent = "failback"
		}
		log.Printf("Detected current DNS pointing to: %s", currentOrigin)
	}

	client := &http.Client{Timeout: cfg.RequestTimeout}
	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	// Run immediately on start
	runCheck(client, cfg, state)

	for range ticker.C {
		runCheck(client, cfg, state)
	}
}

func runCheck(client *http.Client, cfg Config, state *State) {
	state.LastCheck = time.Now()
	healthy := checkTunnelHealth(client, cfg)

	if healthy {
		state.ConsecutiveFails = 0
		state.ConsecutiveOKs++

		if state.CurrentOrigin == "failover" && state.ConsecutiveOKs >= cfg.RecoverThreshold {
			log.Printf("Tunnel recovered (%d consecutive OKs). Switching back to primary.", state.ConsecutiveOKs)
			if switchToPrimary(client, cfg, state) {
				if state.LastSlackEvent != "failback" || time.Since(state.LastSlackTime) > 5*time.Minute {
					state.LastSlackEvent = "failback"
					state.LastSlackTime = time.Now()
					msg := fmt.Sprintf(
						":white_check_mark: *%s failback to PRIMARY*\nPrimary recovered after %s on failover.\nDNS updated to tunnel CNAME.",
						cfg.Domain, time.Since(state.LastSwitch).Round(time.Second),
					)
					sendSlack(cfg.SlackWebhookURL, msg)
				}
			}
		} else if state.CurrentOrigin == "primary" {
			if state.ConsecutiveOKs%10 == 0 {
				log.Printf("Tunnel healthy (check #%d)", state.ConsecutiveOKs)
			}
		}
	} else {
		state.ConsecutiveOKs = 0
		state.ConsecutiveFails++
		log.Printf("Tunnel health check FAILED (%d/%d)", state.ConsecutiveFails, cfg.FailThreshold)

		if state.CurrentOrigin == "primary" && state.ConsecutiveFails >= cfg.FailThreshold {
			log.Printf("Tunnel DOWN (%d consecutive failures). Switching to failover.", state.ConsecutiveFails)
			if switchToFailover(client, cfg, state) {
				if state.LastSlackEvent != "failover" || time.Since(state.LastSlackTime) > 5*time.Minute {
					state.LastSlackEvent = "failover"
					state.LastSlackTime = time.Now()
					msg := fmt.Sprintf(
						":rotating_light: *%s FAILOVER to AWS*\nPrimary failed %d consecutive health checks.\nDNS updated to AWS IP: `%s`\nMonitoring for recovery...",
						cfg.Domain, cfg.FailThreshold, cfg.FailoverIP,
					)
					sendSlack(cfg.SlackWebhookURL, msg)
				}
				if !state.TwilioCallPending && time.Since(state.LastTwilioCall) > 30*time.Minute {
					state.TwilioCallPending = true
					go delayedTwilioCall(cfg, state)
				} else if state.TwilioCallPending {
					log.Printf("Twilio call already pending, skipping")
				} else {
					log.Printf("Twilio cooldown active (last call %s ago), skipping", time.Since(state.LastTwilioCall).Round(time.Second))
				}
			}
		}
	}
}

// checkTunnelHealth checks both HTTP endpoint (via health subdomain) and tunnel API status.
// Both must pass for the primary to be considered healthy.
func checkTunnelHealth(client *http.Client, cfg Config) bool {
	// Check 1: HTTP health check via health.domain (always routes through tunnel)
	httpOK := checkHTTPHealth(client, cfg)

	// Check 2: Tunnel API status
	tunnelOK := checkTunnelAPI(client, cfg)

	if httpOK && tunnelOK {
		log.Printf("Health check OK (http=ok, tunnel=ok)")
		return true
	}

	log.Printf("Health check FAIL (http=%v, tunnel=%v)", httpOK, tunnelOK)
	return false
}

func checkHTTPHealth(client *http.Client, cfg Config) bool {
	req, _ := http.NewRequest("GET", cfg.HealthURL, nil)
	req.Host = cfg.HealthHostHeader

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("HTTP health error: %v", err)
		return false
	}
	defer resp.Body.Close()
	io.ReadAll(io.LimitReader(resp.Body, 1024))

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func checkTunnelAPI(client *http.Client, cfg Config) bool {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s",
		cfg.CFAccountID, cfg.CFTunnelID)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.CFAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Tunnel API error: %v", err)
		return false
	}
	defer resp.Body.Close()

	var cfResp struct {
		Success bool           `json:"success"`
		Result  CFTunnelStatus `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		log.Printf("Tunnel API decode error: %v", err)
		return false
	}

	if !cfResp.Success {
		return false
	}

	return cfResp.Result.Status == "healthy" && len(cfResp.Result.Connections) > 0
}

func detectCurrentOrigin(cfg Config) string {
	records, err := listDNSRecords(cfg)
	if err != nil {
		log.Printf("Could not detect current DNS: %v", err)
		return ""
	}

	for _, r := range records {
		if r.Name != cfg.Domain {
			continue
		}
		if r.Type == "CNAME" && strings.Contains(r.Content, "cfargotunnel") {
			return "primary"
		}
		if r.Type == "A" && r.Content == cfg.FailoverIP {
			return "failover"
		}
	}

	return "primary" // Assume primary if unknown
}

// listDNSRecords returns all DNS records matching the domain
func listDNSRecords(cfg Config) ([]CFDNSRecord, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=CNAME,A",
		cfg.CFZoneID, cfg.Domain)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.CFAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Result []CFDNSRecord `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Result, nil
}

func switchToFailover(client *http.Client, cfg Config, state *State) bool {
	if cfg.DryRun {
		log.Printf("[DRY RUN] Would delete CNAME and create A record: %s -> %s", cfg.Domain, cfg.FailoverIP)
		state.CurrentOrigin = "failover"
		state.LastSwitch = time.Now()
		return true
	}

	// Step 1: Find and delete the current CNAME record
	records, err := listDNSRecords(cfg)
	if err != nil {
		log.Printf("ERROR: Failed to list DNS records: %v", err)
		return false
	}

	for _, r := range records {
		if r.Name == cfg.Domain && r.Type == "CNAME" {
			if err := deleteDNSRecord(cfg, r.ID); err != nil {
				log.Printf("ERROR: Failed to delete CNAME record: %v", err)
				sendSlack(cfg.SlackWebhookURL, fmt.Sprintf(":x: *DNS UPDATE FAILED*\nCould not delete CNAME: %v", err))
				return false
			}
			log.Printf("Deleted CNAME record: %s", r.ID)
		}
	}

	// Step 2: Create A record pointing to failover IP
	record := CFDNSRecord{
		Type:    "A",
		Name:    cfg.Domain,
		Content: cfg.FailoverIP,
		TTL:     60,
		Proxied: true,
	}
	newID, err := createDNSRecord(cfg, record)
	if err != nil {
		log.Printf("ERROR: Failed to create A record: %v", err)
		sendSlack(cfg.SlackWebhookURL, fmt.Sprintf(":x: *DNS UPDATE FAILED*\nCould not create A record: %v", err))
		return false
	}

	state.CurrentOrigin = "failover"
	state.LastSwitch = time.Now()
	state.ConsecutiveFails = 0
	log.Printf("DNS failover complete: A %s -> %s (record: %s, TTL: 60s)", cfg.Domain, cfg.FailoverIP, newID)
	return true
}

func switchToPrimary(client *http.Client, cfg Config, state *State) bool {
	if cfg.DryRun {
		log.Printf("[DRY RUN] Would delete A record and create CNAME: %s -> %s", cfg.Domain, cfg.PrimaryCNAME)
		state.CurrentOrigin = "primary"
		state.LastSwitch = time.Now()
		return true
	}

	// Step 1: Find and delete the current A record
	records, err := listDNSRecords(cfg)
	if err != nil {
		log.Printf("ERROR: Failed to list DNS records: %v", err)
		return false
	}

	for _, r := range records {
		if r.Name == cfg.Domain && r.Type == "A" && r.Content == cfg.FailoverIP {
			if err := deleteDNSRecord(cfg, r.ID); err != nil {
				log.Printf("ERROR: Failed to delete A record: %v", err)
				sendSlack(cfg.SlackWebhookURL, fmt.Sprintf(":x: *DNS FAILBACK FAILED*\nCould not delete A record: %v", err))
				return false
			}
			log.Printf("Deleted A record: %s", r.ID)
		}
	}

	// Step 2: Create CNAME record pointing to tunnel
	record := CFDNSRecord{
		Type:    "CNAME",
		Name:    cfg.Domain,
		Content: cfg.PrimaryCNAME,
		TTL:     1, // Auto
		Proxied: true,
	}
	newID, err := createDNSRecord(cfg, record)
	if err != nil {
		log.Printf("ERROR: Failed to create CNAME record: %v", err)
		sendSlack(cfg.SlackWebhookURL, fmt.Sprintf(":x: *DNS FAILBACK FAILED*\nCould not create CNAME: %v", err))
		return false
	}

	state.CurrentOrigin = "primary"
	state.LastSwitch = time.Now()
	state.ConsecutiveOKs = 0
	log.Printf("DNS failback complete: CNAME %s -> %s (record: %s)", cfg.Domain, cfg.PrimaryCNAME, newID)
	return true
}

func createDNSRecord(cfg Config, record CFDNSRecord) (string, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", cfg.CFZoneID)

	body, _ := json.Marshal(record)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.CFAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var cfResp struct {
		Success bool        `json:"success"`
		Result  CFDNSRecord `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return "", fmt.Errorf("decode failed: %w", err)
	}

	if !cfResp.Success {
		msgs := make([]string, len(cfResp.Errors))
		for i, e := range cfResp.Errors {
			msgs[i] = e.Message
		}
		return "", fmt.Errorf("cloudflare error: %s", strings.Join(msgs, "; "))
	}

	return cfResp.Result.ID, nil
}

func deleteDNSRecord(cfg Config, recordID string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", cfg.CFZoneID, recordID)

	req, _ := http.NewRequest("DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.CFAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	var cfResp CFResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return fmt.Errorf("decode failed: %w", err)
	}

	if !cfResp.Success {
		msgs := make([]string, len(cfResp.Errors))
		for i, e := range cfResp.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("cloudflare error: %s", strings.Join(msgs, "; "))
	}

	return nil
}

func sendSlack(webhookURL, message string) {
	if webhookURL == "" {
		log.Printf("Slack notification (no webhook): %s", message)
		return
	}

	payload, _ := json.Marshal(map[string]string{"text": message})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("Slack notification failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Slack notification error: %d %s", resp.StatusCode, string(body))
	}
}

// delayedTwilioCall waits 5 minutes, then calls if still on failover.
func delayedTwilioCall(cfg Config, state *State) {
	defer func() { state.TwilioCallPending = false }()

	delay := 5 * time.Minute
	log.Printf("Twilio call scheduled in %s (will cancel if primary recovers)", delay)
	time.Sleep(delay)

	if state.CurrentOrigin != "failover" {
		log.Printf("Primary recovered before call delay, skipping Twilio call")
		return
	}

	log.Printf("Still on failover after %s, initiating calls", delay)
	state.LastTwilioCall = time.Now()
	twilioCallChain(cfg, fmt.Sprintf("%s has been on failover for %s", cfg.Domain, delay))
}

// twilioCallChain calls each number in order. If one answers, stop.
func twilioCallChain(cfg Config, message string) {
	if cfg.TwilioAccountSID == "" || len(cfg.TwilioToNumbers) == 0 {
		log.Printf("Twilio not configured, skipping call")
		return
	}

	for _, number := range cfg.TwilioToNumbers {
		log.Printf("Calling %s...", number)
		answered, err := twilioCall(cfg, number, message)
		if err != nil {
			log.Printf("Twilio call to %s failed: %v", number, err)
			continue
		}
		if answered {
			log.Printf("Call answered by %s", number)
			return
		}
		log.Printf("No answer from %s, trying next...", number)
	}
	log.Printf("All Twilio calls exhausted, no one answered")
}

func twilioCall(cfg Config, toNumber, message string) (bool, error) {
	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls.json", cfg.TwilioAccountSID)

	twiml := `<Response><Play>https://octo-2801.twil.io/voice.mp3</Play><Pause length="1"/><Play>https://octo-2801.twil.io/voice.mp3</Play></Response>`

	data := url.Values{}
	data.Set("To", toNumber)
	data.Set("From", cfg.TwilioFromNumber)
	data.Set("Twiml", twiml)
	data.Set("Timeout", "30")
	data.Set("MachineDetection", "Enable")

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	req.SetBasicAuth(cfg.TwilioAccountSID, cfg.TwilioAuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 201 {
		return false, fmt.Errorf("twilio error %d: %s", resp.StatusCode, string(body))
	}

	var callResp struct {
		SID string `json:"sid"`
	}
	json.Unmarshal(body, &callResp)
	log.Printf("Twilio call initiated: SID=%s to=%s", callResp.SID, toNumber)

	// Poll call status every 3 seconds
	callURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls/%s.json", cfg.TwilioAccountSID, callResp.SID)
	for i := 0; i < 20; i++ { // max 60 seconds
		time.Sleep(3 * time.Second)

		statusReq, _ := http.NewRequest("GET", callURL, nil)
		statusReq.SetBasicAuth(cfg.TwilioAccountSID, cfg.TwilioAuthToken)

		statusResp, err := http.DefaultClient.Do(statusReq)
		if err != nil {
			continue
		}

		var status struct {
			Status     string `json:"status"`
			AnsweredBy string `json:"answered_by"`
		}
		json.NewDecoder(statusResp.Body).Decode(&status)
		statusResp.Body.Close()

		log.Printf("Call to %s: status=%s answered_by=%s", toNumber, status.Status, status.AnsweredBy)

		// Voicemail detected - hang up immediately and try next
		if strings.Contains(status.AnsweredBy, "machine") {
			log.Printf("Voicemail detected on %s, hanging up", toNumber)
			cancelReq, _ := http.NewRequest("POST", callURL, strings.NewReader("Status=completed"))
			cancelReq.SetBasicAuth(cfg.TwilioAccountSID, cfg.TwilioAuthToken)
			cancelReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			http.DefaultClient.Do(cancelReq)
			return false, nil
		}

		// Human answered and call completed
		if status.Status == "completed" {
			return true, nil
		}

		// Call failed/busy/no-answer
		if status.Status == "failed" || status.Status == "busy" || status.Status == "no-answer" || status.Status == "canceled" {
			return false, nil
		}
	}

	// Timeout - cancel and move on
	log.Printf("Call to %s timed out, canceling", toNumber)
	cancelReq, _ := http.NewRequest("POST", callURL, strings.NewReader("Status=completed"))
	cancelReq.SetBasicAuth(cfg.TwilioAccountSID, cfg.TwilioAuthToken)
	cancelReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	http.DefaultClient.Do(cancelReq)
	return false, nil
}

func parseTwilioNumbers(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var numbers []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			numbers = append(numbers, p)
		}
	}
	return numbers
}

// Helper functions
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required env var %s is not set", key)
	}
	return v
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var result int
	fmt.Sscanf(v, "%d", &result)
	if result == 0 {
		return fallback
	}
	return result
}
