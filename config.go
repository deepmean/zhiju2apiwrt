package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole runtime configuration. Everything is overridable from the
// environment, and optionally from a KEY=VALUE file (default /opt/zhiju2api/zhiju2api.conf)
// so a procd init script can stay trivial.
type Config struct {
	Listen    string
	APIKeys   []string
	LogLevel  string
	Upstream  string // https://agent.zhijuai.com
	TokensF   string // session tokens, one per line
	ModelMapF string // model_key=model_id overrides, one per line

	// Upstream protocol versions. The web client sends different values per endpoint:
	// the v2 chat/turns route needs channelConfig.wholeApiVersion (3.2.0 as of writing),
	// while the aimoss v1 routes accept 2.2.11 and the repair routes 1.1.10.
	ChatVer    string
	AimossVer  string
	RepairVer  string
	Discover   bool // try to discover ChatVer from /v1/aimoss/cfg/list at boot

	FastModel        bool // the web UI defaults to fast mode; slow lane answers 10033
	MaxContextRounds int
	// Resolution tier handed to the image routes. This is a PREFERENCE, not a switch:
	// each model declares its own tiers upstream (most declare exactly one), so the value
	// is used only when the model lists it, and the model's own default otherwise.
	ImageSize string
	// Cost guard. The estimate is always fetched from the upstream billing module and
	// logged/annotated; a ceiling above zero additionally refuses requests over it.
	MaxCostPerRequest int
	PriceCacheTTL     time.Duration
	ParamsCacheTTL    time.Duration

	// Midjourney is NOT metered in 灵力: the billing module has no price row for it (the
	// lookup answers with a generic error) and the account catalog shows remain_count=-1.
	// "No 灵力 price" does NOT mean free — it is the most expensive model on the account,
	// billed outside this meter. It is therefore off unless explicitly enabled, so it can
	// never be reached by a typo or a client that picks the first model in the list.
	MJEnabled bool

	ChatMaxConcurrent  int
	ImageMaxConcurrent int

	ChatTimeout  time.Duration
	ImageTimeout time.Duration
	PollInterval time.Duration

	HeartbeatInterval time.Duration
	TokenReloadEvery  time.Duration
	ModelCacheTTL     time.Duration

	MaxBodyBytes   int64
	MaxImageBytes  int64
	MaxUploadBytes int64
}

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

// loadConfFile applies KEY=VALUE lines from path to the environment, without clobbering
// variables that are already set — an explicit env var always wins over the file.
func loadConfFile(path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func LoadConfig() *Config {
	loadConfFile(envStr("CONF_FILE", "/opt/zhiju2api/zhiju2api.conf"))

	keys := []string{}
	for _, k := range strings.Split(envStr("API_KEY", ""), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}

	return &Config{
		Listen:    envStr("LISTEN", "0.0.0.0:8080"),
		APIKeys:   keys,
		LogLevel:  strings.ToUpper(envStr("LOG_LEVEL", "INFO")),
		Upstream:  strings.TrimRight(envStr("UPSTREAM", "https://agent.zhijuai.com"), "/"),
		TokensF:   envStr("TOKENS_FILE", "/opt/zhiju2api/tokens.txt"),
		ModelMapF: envStr("MODEL_MAP_FILE", "/opt/zhiju2api/models.map"),

		ChatVer:   envStr("CHAT_VER", "3.2.0"),
		AimossVer: envStr("AIMOSS_VER", "2.2.11"),
		RepairVer: envStr("REPAIR_VER", "1.1.10"),
		Discover:  envBool("DISCOVER_VERSION", true),

		FastModel:        envBool("FAST_MODEL", true),
		MaxContextRounds: envInt("MAX_CONTEXT_ROUNDS", 5),
		ImageSize:        envStr("IMAGE_SIZE", "2K"),
		MJEnabled:        envBool("MJ_ENABLED", false),
		// 0 = annotate only (the default): every request logs and reports its estimated
		// cost, nothing is refused.
		MaxCostPerRequest: envInt("MAX_COST_PER_REQUEST", 0),
		PriceCacheTTL:     envDur("PRICE_CACHE_TTL", 10*time.Minute),
		ParamsCacheTTL:    envDur("PARAMS_CACHE_TTL", 30*time.Minute),

		ChatMaxConcurrent:  envInt("CHAT_MAX_CONCURRENT", 4),
		ImageMaxConcurrent: envInt("IMAGE_MAX_CONCURRENT", 1),

		ChatTimeout:  envDur("CHAT_TIMEOUT", 5*time.Minute),
		ImageTimeout: envDur("IMAGE_TIMEOUT", 5*time.Minute),
		PollInterval: envDur("POLL_INTERVAL", 4*time.Second),

		HeartbeatInterval: envDur("HEARTBEAT_INTERVAL", 30*time.Minute),
		TokenReloadEvery:  envDur("TOKEN_RELOAD_INTERVAL", 5*time.Second),
		ModelCacheTTL:     envDur("MODEL_CACHE_TTL", 30*time.Minute),

		MaxBodyBytes:   int64(envInt("MAX_BODY_BYTES", 8<<20)),
		MaxImageBytes:  int64(envInt("MAX_IMAGE_BYTES", 8<<20)),
		MaxUploadBytes: int64(envInt("MAX_UPLOAD_BYTES", 10<<20)),
	}
}
