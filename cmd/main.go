package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aukilabs/go-tooling/pkg/cli"
	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/events"
	"github.com/aukilabs/go-tooling/pkg/logs"
	"github.com/aukilabs/go-tooling/pkg/metrics"
	"github.com/aukilabs/hagall-common/crypt"
	hds "github.com/aukilabs/hagall-common/hdsclient"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/ncsclient"
	hsmoketest "github.com/aukilabs/hagall-common/smoketest"
	"github.com/aukilabs/hagall/featureflag"
	hagallhttp "github.com/aukilabs/hagall/http"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
	"github.com/aukilabs/hagall/modules/dagaz"
	"github.com/aukilabs/hagall/modules/odal"
	"github.com/aukilabs/hagall/modules/vikja"
	"github.com/aukilabs/hagall/receipt"
	"github.com/aukilabs/hagall/smoketest"
	hwebsocket "github.com/aukilabs/hagall/websocket"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/encoding/json"
	"golang.org/x/net/websocket"
)

var (
	// The Hagall version number. Set at build.
	version = "v0.5.0"

	infoGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name:        "hagall_info",
		Help:        "Hagall information.",
		ConstLabels: prometheus.Labels{"version": version},
	})
)

// This will effectively disable obfuscation of the config struct. Without it, the keys would get obfuscated causing the cli package to generate garbled command-line options.
// https://github.com/burrowers/garble/issues/403
var _ = reflect.TypeOf(config{})

type config struct {
	Addr               string        `cli:""        env:"HAGALL_ADDR"                  help:"Listening address for client connections."`
	AdminAddr          string        `cli:""        env:"HAGALL_ADMIN_ADDR"            help:"Admin listening address."`
	PublicEndpoint     string        `cli:""        env:"HAGALL_PUBLIC_ENDPOINT"       help:"The public endpoint where this Hagall server is reachable."`
	PrivateKey         string        `cli:""        env:"HAGALL_PRIVATE_KEY"           help:"The private key of a Hagall server-unique Ethereum-compatible wallet."`
	PrivateKeyFile     string        `cli:""        env:"HAGALL_PRIVATE_KEY_FILE"      help:"The file that contains the private key of a Hagall server-unique Ethereum-compatible wallet."`
	LogLevel           string        `cli:""        env:"HAGALL_LOG_LEVEL"             help:"Log level (debug|info|warning|error)."`
	LogIndent          bool          `cli:""        env:"HAGALL_LOG_INDENT"            help:"Indent logs."`
	SyncClockInterval  time.Duration `cli:",hidden" env:"HAGALL_SYNC_CLOCK_INTERVAL"   help:"Client sync clock (heartbeat) message interval."`
	ClientIdleTimeout  time.Duration `cli:",hidden" env:"HAGALL_CLIENT_IDLE_TIMEOUT"   help:"Time until an idle client will be disconnected"`
	FrameDuration      time.Duration `cli:",hidden" env:"HAGALL_FRAME_DURATION"        help:"The duration of a session frame."`
	LogSummaryInterval time.Duration `cli:",hidden" env:"HAGALL_LOG_SUMMARY_INTERVAL"  help:"The duration between each log summary by connection."`
	HDS                hdsConfig     `cli:",hidden" env:"-"                            help:"HDS configuration."`
	Events             eventsConfig  `cli:",hidden" env:"-"                            help:"Event pusher configuration."`
	FeatureFlags       []string      `cli:",hidden" env:"HAGALL_FEATURE_FLAGS"         help:"Comma separated feature flags"`
	NCSEndpoint        string        `cli:",hidden" env:"HAGALL_NCS_ENDPOINT"          help:"Network Credit Service Endpoint."`
	Version            bool          `cli:""        env:"-"                            help:"Show version."`
	Help               bool          `cli:""        env:"-"                            help:"Show help."`
}

type hdsConfig struct {
	Endpoint             string        `cli:",hidden" env:"HAGALL_HDS_ENDPOINT"              help:"HDS enpoint."`
	RegistrationInterval time.Duration `cli:",hidden" env:"HAGALL_HDS_REGISTRATION_INTERVAL" help:"The duration between each HDS registration try."`
	HealthCheckTTL       time.Duration `cli:",hidden" env:"HAGALL_HDS_HEALTHCHECK_TTL"       help:"The elapsed time required since the last health check to trigger a new registration."`
	RegistrationRetries  int           `cli:",hidden" env:"HAGALL_HDS_REGISTRATION_RETRIES"  help:"The number of registration retries."`
}

type eventsConfig struct {
	Endpoint      string        `cli:",hidden" env:"HAGALL_EVENTS_ENDPOINT"       help:"Endpoint to where events are pushed."`
	FlushInterval time.Duration `cli:",hidden" env:"HAGALL_EVENTS_FLUSH_INTERVAL" help:"The duration between each event flush."`
	BatchSize     int           `cli:",hidden" env:"HAGALL_EVENTS_BATCH_SIZE"     help:"The maximum number of events sent at once."`
	QueueSize     int           `cli:",hidden" env:"HAGALL_EVENTS_QUEUE_SIZE"     help:"The size of the queue where events are stored."`
}

func main() {
	conf := config{
		Addr:               ":4000",
		AdminAddr:          ":18190",
		PublicEndpoint:     "http://localhost:4000",
		LogLevel:           logs.InfoLevel.String(),
		SyncClockInterval:  time.Second * 5,
		ClientIdleTimeout:  time.Minute * 5,
		FrameDuration:      time.Millisecond * 15,
		LogSummaryInterval: time.Minute,
		HDS: hdsConfig{
			Endpoint:             "https://hds.posemesh.org",
			RegistrationInterval: time.Second * 15,
			HealthCheckTTL:       time.Minute * 2,
			RegistrationRetries:  3,
		},
		Events: eventsConfig{
			Endpoint:      "https://znw4vaxw00.execute-api.us-east-1.amazonaws.com/log-prod_serverless_lambda_stage/log",
			FlushInterval: events.DefaultFlushInterval,
			BatchSize:     events.DefaultBatchSize,
			QueueSize:     events.DefaultQueueSize,
		},
		NCSEndpoint: "http://localhost:4040",
	}

	// set the information gauge to 1, useful for SUM query
	infoGauge.Set(1)

	ctx, cancel := cli.ContextWithSignals(context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancel()

	cli.Register().
		Help("Starts Hagall server.").
		Options(&conf)
	cli.Load()

	if conf.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	if err := validateConfig(conf); err != nil {
		logs.Fatal(err)
	}

	privateKey, err := loadPrivateKey(conf)
	if err != nil {
		logs.Fatal(errors.New("error loading private key").Wrap(err))
	}

	logs.SetLevel(logs.ParseLevel(conf.LogLevel))
	logs.Encoder = json.Marshal
	if conf.LogIndent {
		logs.Encoder = func(v any) ([]byte, error) {
			return json.MarshalIndent(v, "", "  ")
		}
	}

	errors.Encoder = json.Marshal

	transport := metrics.HTTPTransport(http.DefaultTransport)

	if conf.Events.Endpoint != "" {
		eventsPusher := events.Pusher{
			Endpoint:      conf.Events.Endpoint,
			FlushInterval: conf.Events.FlushInterval,
			BatchSize:     conf.Events.BatchSize,
			QueueSize:     conf.Events.QueueSize,
			Transport:     transport,
		}
		go eventsPusher.Start()
		defer eventsPusher.Close()

		eventsLogger := events.Logger{
			Pusher:           &eventsPusher,
			SDKType:          "hagall",
			SDKVersionFamily: version,
		}
		logs.SetLogger(eventsLogger.Log)
	}

	var service http.ServeMux

	hdsClient := hds.NewClient(hds.WithHagallEndpoint(conf.PublicEndpoint),
		hds.WithHDSEndpoint(conf.HDS.Endpoint),
		hds.WithEncoder(json.Marshal),
		hds.WithTransport(transport),
		hds.WithDecoder(json.Unmarshal),
		hds.WithPrivateKey(privateKey),
	)

	service.HandleFunc("/registrations", hdsClient.HandleServerRegistration)
	service.Handle("/health", hagallhttp.HandleWithCORS(http.HandlerFunc(hdsClient.HandleHealthCheck)))
	service.Handle("/version", hagallhttp.HandleWithCORS(http.HandlerFunc(hagallhttp.HandleVersion(version))))
	service.Handle("/pms/metrics", crypt.HandleWithEncryption(
		crypt.NewHagallSecretProvider(hdsClient),
		promhttp.Handler()))

	service.HandleFunc("/smoke-test", hagallhttp.VerifyAuthTokenHandler(hdsClient, smoketest.HandleSmokeTest(ctx, smoketest.Options{
		Endpoint:              conf.PublicEndpoint,
		UserAgent:             fmt.Sprintf("Hagall %s", version),
		MakeHagallServerToken: httpcmn.GenerateHagallUserAccessToken,
		SendResult: func(ctx context.Context, res hsmoketest.SmokeTestResults) error {
			return hdsClient.SendSmokeTestResult(ctx, res)
		},
	})))

	readinessCheck := func() bool {
		return hdsClient.GetRegistrationStatus() == hds.RegistrationStatusRegistered
	}
	service.Handle("/ready", hagallhttp.HandleWithCORS(http.HandlerFunc(hagallhttp.HandleReadyCheck(readinessCheck))))

	sessions := models.SessionStore{
		DiscoveryService: hdsClient,
	}

	receiptChan := make(chan ncsclient.ReceiptPayload, 128)
	receiptHandler := receipt.ReceiptHandler{
		NCSEndpoint: conf.NCSEndpoint,
		ReceiptChan: receiptChan,
	}
	receiptHandler.HandleReceipts(ctx)

	service.Handle("/", hagallhttp.HandleWithCORS(websocket.Server{
		Handshake: hagallhttp.VerifyAuthToken(ctx, hdsClient),
		Handler: func(conn *websocket.Conn) {
			defer conn.Close()

			var rh hwebsocket.Handler = &hwebsocket.RealtimeHandler{
				ClientSyncClockInterval: conf.SyncClockInterval,
				ClientIdleTimeout:       conf.ClientIdleTimeout,
				FrameDuration:           conf.FrameDuration,
				Sessions:                &sessions,
				Modules: []modules.Module{
					&vikja.Module{},
					&odal.Module{},
					&dagaz.Module{},
				},
				FeatureFlags: featureflag.New(conf.FeatureFlags),
				ReceiptChan:  receiptChan,
			}
			h := hwebsocket.HandlerWithLogs(rh, conf.LogSummaryInterval)
			h = hwebsocket.HandlerWithMetrics(h, conf.PublicEndpoint)
			defer h.Close()

			hwebsocket.Handle(ctx, conn, h)
		},
	}))

	service.Handle("/ping", websocket.Server{
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			io.Copy(ws, ws)
		},
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := pairWithHDS(ctx, hdsClient, conf)
		if err != nil && err != context.Canceled {
			logs.Fatal(errors.New("registering with HDS failed").Wrap(err))
		}
	}()

	var admin http.ServeMux
	admin.Handle("/metrics", promhttp.Handler())
	admin.HandleFunc("/health", hagallhttp.HandleHealthCheck)
	admin.HandleFunc("/debug/pprof/", pprof.Index)
	admin.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	admin.HandleFunc("/debug/pprof/profile", pprof.Profile)
	admin.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	admin.HandleFunc("/debug/pprof/trace", pprof.Trace)
	admin.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	admin.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	admin.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	admin.Handle("/debug/pprof/block", pprof.Handler("block"))
	admin.HandleFunc("/ready", hagallhttp.HandleReadyCheck(readinessCheck))

	walletAddress := strings.ToLower(crypto.PubkeyToAddress(privateKey.PublicKey).Hex())
	logs.WithTag("version", version).
		WithTag("log_level", conf.LogLevel).
		WithTag("endpoint", conf.PublicEndpoint).
		WithTag("wallet_address", walletAddress).
		Info("starting hagall server")

	hagallhttp.ListenAndServe(ctx,
		&http.Server{Addr: conf.Addr, Handler: metrics.HTTPHandler(&service,
			hagallhttp.MetricsPathFormatter)},
		&http.Server{Addr: conf.AdminAddr, Handler: &admin},
	)

	wg.Wait()
	// unpair on exit
	if err = hdsClient.Unpair(); err != nil {
		logs.Warn(errors.New("unpair with hds failed").Wrap(err))
	}
}

func pairWithHDS(ctx context.Context, c *hds.Client, conf config) error {
	return c.Pair(ctx, hds.PairIn{
		Endpoint:             conf.PublicEndpoint,
		RegistrationInterval: conf.HDS.RegistrationInterval,
		HealthCheckTTL:       conf.HDS.HealthCheckTTL,
		RegistrationRetries:  conf.HDS.RegistrationRetries,
		Version:              version,
		Modules: []string{
			(&vikja.Module{}).Name(),
			(&odal.Module{}).Name(),
			(&dagaz.Module{}).Name(),
		},
		FeatureFlags: conf.FeatureFlags,
	})
}

func loadPrivateKey(conf config) (*ecdsa.PrivateKey, error) {
	privateKey := conf.PrivateKey

	if len(conf.PrivateKeyFile) != 0 {
		privateKeyBytes, err := os.ReadFile(conf.PrivateKeyFile)
		if err != nil {
			return nil, errors.New("error loading private key from file").
				WithTag("file_name", conf.PrivateKeyFile).
				Wrap(err)
		}
		privateKey = string(privateKeyBytes)
	}

	privateKey = strings.TrimPrefix(strings.TrimSpace(privateKey), "0x")

	if len(privateKey) == 0 {
		return nil, errors.New("private key is empty")
	}

	return crypto.HexToECDSA(privateKey)
}

func validateConfig(conf config) error {
	if _, err := url.ParseRequestURI(conf.PublicEndpoint); err != nil {
		return errors.New("invalid public endpoint").Wrap(err)
	}

	if len(conf.PrivateKey) != 0 &&
		len(conf.PrivateKeyFile) != 0 {
		return errors.New("have to specify either private key or private key file, not both")
	}

	if len(conf.PrivateKey) == 0 &&
		len(conf.PrivateKeyFile) == 0 {
		return errors.New("have to specify either private key or private key file")
	}

	return nil
}
