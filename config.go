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
	// Ledger of the upstream sessions this gateway created, so the cloud records it
	// leaves behind can be deleted precisely (see /admin/records).
	RecordsFile string

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

	// Daily credit tasks: the app-side check-in (POST /v1/aimoss/task/sign, once a day,
	// +50 灵力 measured) plus any task the account lists. The web frontend never calls
	// these, but they answer web headers.
	SignInEnabled bool
	SignInAt      string // local HH:MM

	ChatMaxConcurrent  int
	ImageMaxConcurrent int

	ChatTimeout  time.Duration
	ImageTimeout time.Duration
	PollInterval time.Duration

	HeartbeatInterval time.Duration
	TokenReloadEvery  time.Duration
	ModelCacheTTL     time.Duration
	// How long before a token's exp the heartbeat starts warning about it. There is no
	// renewal upstream, so expiry is a deadline the operator has to meet.
	TokenExpiryWarnDays int
	// A file the verification code can be dropped into during a pending login, so the
	// code can arrive from an SMS forwarder or a single ssh command instead of a JSON
	// request. Empty disables the watcher.
	SMSCodeFile string
	// PromptLimitMode decides what the upstream-declared prompt_limit means.
	//
	// The upstream returns prompt_limit=1000 for every image model, but it does not
	// enforce it: prompts of 1980 and 8000 characters were both accepted, and the site's
	// own image pages never read the field (only the video page does, with a `|| 1000`
	// fallback). Since no ceiling could be found, the gateway does not invent one — the
	// upstream's own answer is the authority.
	//
	//	off     ignore the field entirely (default)
	//	warn    log a line and send anyway
	//	enforce reject with 400
	PromptLimitMode string
	// StrictParams turns the aspect-ratio / resolution checks into rejections.
	//
	// Off by default for the same reason: the values are declared per model, but a value
	// the model does not list has not been shown to be refused by the upstream, so the
	// request is passed through and the upstream's error is returned verbatim. With it on,
	// an unsupported value is a 400 that lists what the model declares.
	StrictParams bool

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
		RecordsFile: envStr("RECORDS_FILE", "/opt/zhiju2api/api-records.jsonl"),

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
		TokenExpiryWarnDays: envInt("TOKEN_EXPIRY_WARN_DAYS", 14),
		SMSCodeFile:         envStr("SMS_CODE_FILE", "/tmp/zhiju-sms-code"),
		PromptLimitMode:     strings.ToLower(envStr("PROMPT_LIMIT_MODE", "off")),
		StrictParams:        envBool("STRICT_PARAMS", false),
		SignInEnabled:       envBool("SIGNIN_ENABLED", true),
		SignInAt:            envStr("SIGNIN_AT", "00:05"),

		MaxBodyBytes:   int64(envInt("MAX_BODY_BYTES", 8<<20)),
		MaxImageBytes:  int64(envInt("MAX_IMAGE_BYTES", 8<<20)),
		MaxUploadBytes: int64(envInt("MAX_UPLOAD_BYTES", 10<<20)),
	}
}
