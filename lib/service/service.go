package service

import (
	"path"
	"time"

	"github.com/gravitational/teleport/lib/auth"
	authority "github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk/encryptor"
	"github.com/gravitational/teleport/lib/backend/etcdbk"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/events/boltlog"
	"github.com/gravitational/teleport/lib/recorder"
	"github.com/gravitational/teleport/lib/recorder/boltrec"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/tun"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/codahale/lunk"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/log"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/trace"
	oxytrace "github.com/gravitational/teleport/Godeps/_workspace/src/github.com/mailgun/oxy/trace"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
)

type TeleportService struct {
	Supervisor
}

func NewTeleport(cfg Config) (*TeleportService, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	setDefaults(&cfg)

	// if user started auth and something else and did not
	// provide auth address for that something,
	// the address of the created auth will be used
	if cfg.Auth.Enabled && len(cfg.AuthServers) == 0 {
		cfg.AuthServers = []utils.NetAddr{cfg.Auth.SSHAddr}
	}

	if err := initLogging(cfg.Log.Output, cfg.Log.Severity); err != nil {
		return nil, err
	}

	t := &TeleportService{}
	t.Supervisor = *New()

	if cfg.Auth.Enabled {
		if err := initAuth(t, cfg); err != nil {
			return nil, err
		}
	}

	if cfg.SSH.Enabled {
		if err := initSSH(t, cfg); err != nil {
			return nil, err
		}
	}

	if cfg.Tun.Enabled {
		if err := initTun(t, cfg); err != nil {
			return nil, err
		}
	}

	return t, nil
}

func initAuth(t *TeleportService, cfg Config) error {
	if cfg.DataDir == "" {
		return trace.Errorf("please supply data directory")
	}
	if cfg.Auth.Domain == "" {
		return trace.Errorf("please provide auth domain, e.g. example.com")
	}

	b, err := initBackend(cfg)
	if err != nil {
		return err
	}

	elog, err := initEventBackend(
		cfg.Auth.EventsBackend.Type, cfg.Auth.EventsBackend.Params)
	if err != nil {
		return err
	}

	rec, err := initRecordBackend(
		cfg.Auth.RecordsBackend.Type, cfg.Auth.RecordsBackend.Params)
	if err != nil {
		return err
	}
	asrv, signer, err := auth.Init(auth.InitConfig{
		Backend:                b,
		Authority:              authority.New(),
		FQDN:                   cfg.FQDN,
		AuthDomain:             cfg.Auth.Domain,
		DataDir:                cfg.DataDir,
		SecretKey:              cfg.Auth.SecretKey,
		AllowedTokens:          cfg.Auth.AllowedTokens,
		TrustedUserAuthorities: cfg.Auth.TrustedUserAuthorities,
	})
	if err != nil {
		log.Errorf("failed to init auth server: %v", err)
		return err
	}

	// register HTTP API endpoint
	t.RegisterFunc(func() error {
		apisrv := auth.NewAPIServer(asrv, elog, session.New(b), rec)
		t, err := oxytrace.New(apisrv, log.GetLogger().Writer(log.SeverityInfo))
		if err != nil {
			log.Fatalf("failed to start: %v", err)
		}

		log.Infof("teleport http authority starting on %v", cfg.Auth.HTTPAddr)
		return utils.StartHTTPServer(cfg.Auth.HTTPAddr, t)
	})

	// register auth SSH-based endpoint
	t.RegisterFunc(func() error {
		tsrv, err := auth.NewTunServer(
			cfg.Auth.SSHAddr, []ssh.Signer{signer},
			cfg.Auth.HTTPAddr,
			asrv)
		if err != nil {
			log.Errorf("failed to start teleport ssh tunnel")
			return err
		}
		if err := tsrv.Start(); err != nil {
			log.Errorf("failed to start teleport ssh endpoint: %v", err)
			return err
		}
		return nil
	})
	return nil
}

func initSSH(t *TeleportService, cfg Config) error {
	if cfg.DataDir == "" {
		return trace.Errorf("please supply data directory")
	}
	if len(cfg.AuthServers) == 0 {
		return trace.Errorf("supply at least one auth server")
	}
	haveKeys, err := auth.HaveKeys(cfg.FQDN, cfg.DataDir)
	if err != nil {
		return err
	}
	if !haveKeys {
		// this means the server has not been initialized yet we are starting
		// the registering client that attempts to connect ot the auth server
		// and provision the keys
		return initRegister(t, cfg.SSH.Token, cfg, initSSHEndpoint)
	}
	return initSSHEndpoint(t, cfg)
}

func initSSHEndpoint(t *TeleportService, cfg Config) error {
	signer, err := auth.ReadKeys(cfg.FQDN, cfg.DataDir)

	client, err := auth.NewTunClient(
		cfg.AuthServers[0],
		cfg.FQDN,
		[]ssh.AuthMethod{ssh.PublicKeys(signer)})

	elog := &FanOutEventLogger{
		Loggers: []lunk.EventLogger{
			lunk.NewTextEventLogger(log.GetLogger().Writer(log.SeverityInfo)),
			client,
		}}

	s, err := srv.New(cfg.SSH.Addr,
		[]ssh.Signer{signer},
		client,
		srv.SetShell(cfg.SSH.Shell),
		srv.SetEventLogger(elog),
		srv.SetSessionServer(client),
		srv.SetRecorder(client))
	if err != nil {
		return err
	}

	t.RegisterFunc(func() error {
		log.Infof("teleport ssh starting on %v", cfg.SSH.Addr)
		if err := s.Start(); err != nil {
			log.Fatalf("failed to start: %v", err)
			return err
		}
		s.Wait()
		return nil
	})
	return nil
}

func initRegister(t *TeleportService, token string, cfg Config,
	initFunc func(*TeleportService, Config) error) error {
	// we are on the same server as the auth endpoint
	// and there's no token. we can handle this
	if cfg.Auth.Enabled && token == "" {
		log.Infof("registering in embedded mode, connecting to local auth server")
		clt, err := auth.NewClientFromNetAddr(cfg.Auth.HTTPAddr)
		if err != nil {
			log.Errorf("failed to instantiate client: %v", err)
			return err
		}
		token, err = clt.GenerateToken(cfg.FQDN, 30*time.Second)
		if err != nil {
			log.Errorf("failed to generate token: %v", err)
		}
		return err
	}
	t.RegisterFunc(func() error {
		log.Infof("teleport:register connecting to auth servers %v", cfg.SSH.Token)
		if err := auth.Register(
			cfg.FQDN, cfg.DataDir, token, cfg.AuthServers); err != nil {
			log.Errorf("teleport:ssh register failed: %v", err)
			return err
		}
		log.Infof("teleport:register registered successfully")
		return initFunc(t, cfg)
	})
	return nil
}

func initTun(t *TeleportService, cfg Config) error {
	if cfg.DataDir == "" {
		return trace.Errorf("please supply data directory")
	}
	if len(cfg.AuthServers) == 0 {
		return trace.Errorf("supply at least one auth server")
	}
	haveKeys, err := auth.HaveKeys(cfg.FQDN, cfg.DataDir)
	if err != nil {
		return err
	}
	if !haveKeys {
		// this means the server has not been initialized yet we are starting
		// the registering client that attempts to connect ot the auth server
		// and provision the keys
		return initRegister(t, cfg.Tun.Token, cfg, initTunAgent)
	}
	return initTunAgent(t, cfg)
}

func initTunAgent(t *TeleportService, cfg Config) error {
	signer, err := auth.ReadKeys(cfg.FQDN, cfg.DataDir)

	client, err := auth.NewTunClient(
		cfg.AuthServers[0],
		cfg.FQDN,
		[]ssh.AuthMethod{ssh.PublicKeys(signer)})

	elog := &FanOutEventLogger{
		Loggers: []lunk.EventLogger{
			lunk.NewTextEventLogger(log.GetLogger().Writer(log.SeverityInfo)),
			client,
		}}

	a, err := tun.NewAgent(
		cfg.Tun.ServerAddr,
		cfg.FQDN,
		[]ssh.Signer{signer},
		client,
		tun.SetEventLogger(elog))
	if err != nil {
		return err
	}

	t.RegisterFunc(func() error {
		log.Infof("teleport ws agent starting")
		if err := a.Start(); err != nil {
			log.Fatalf("failed to start: %v", err)
			return err
		}
		a.Wait()
		return nil
	})
	return nil
}

func initBackend(cfg Config) (*encryptedbk.ReplicatedBackend, error) {
	var bk backend.Backend
	var err error

	switch cfg.Auth.KeysBackend.Type {
	case "etcd":
		bk, err = etcdbk.FromObject(cfg.Auth.KeysBackend.Params)
	case "bolt":
		bk, err = boltbk.FromObject(cfg.Auth.KeysBackend.Params)
	default:
		return nil, trace.Errorf("unsupported backend type: %v", cfg.Auth.KeysBackend.Type)
	}
	if err != nil {
		log.Errorf(err.Error())
		return nil, err
	}

	keyStorage := path.Join(cfg.DataDir, "backend_keys")
	addKeys := []encryptor.Key{}
	if len(cfg.Auth.KeysBackend.AdditionalKey) != 0 {
		addKey, err := encryptedbk.LoadKeyFromFile(cfg.Auth.KeysBackend.AdditionalKey)
		if err != nil {
			return nil, err
		}
		addKeys = append(addKeys, addKey)
	}

	encryptedBk, err := encryptedbk.NewReplicatedBackend(bk,
		keyStorage, addKeys)

	if err != nil {
		log.Errorf(err.Error())
		log.Infof("Initializing backend as follower node")
		myKey, err := encryptor.GenerateGPGKey(cfg.FQDN + " key")
		if err != nil {
			return nil, err
		}
		masterKey, err := auth.RegisterNewAuth(cfg.FQDN, cfg.Auth.Token,
			myKey.Public(), cfg.AuthServers)
		if err != nil {
			return nil, err
		}
		log.Infof(" ", myKey, masterKey)
		encryptedBk, err = encryptedbk.NewReplicatedBackend(bk,
			keyStorage, []encryptor.Key{myKey, masterKey})
		if err != nil {
			return nil, err
		}
	}
	return encryptedBk, nil
}

func initEventBackend(btype string, params interface{}) (events.Log, error) {
	switch btype {
	case "bolt":
		return boltlog.FromObject(params)
	}
	return nil, trace.Errorf("unsupported backend type: %v", btype)
}

func initRecordBackend(btype string, params interface{}) (recorder.Recorder, error) {
	switch btype {
	case "bolt":
		return boltrec.FromObject(params)
	}
	return nil, trace.Errorf("unsupported backend type: %v", btype)
}

func initLogging(ltype, severity string) error {
	return log.Initialize(ltype, severity)
}

func validateConfig(cfg Config) error {
	if !cfg.Auth.Enabled && !cfg.SSH.Enabled && !cfg.Tun.Enabled {
		return trace.Errorf("supply at least one of Auth, SSH or Tun roles")
	}
	return nil
}

type FanOutEventLogger struct {
	Loggers []lunk.EventLogger
}

func (f *FanOutEventLogger) Log(id lunk.EventID, e lunk.Event) {
	for _, l := range f.Loggers {
		l.Log(id, e)
	}
}