package manager

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	r "github.com/dancannon/gorethink"
	"github.com/gorilla/sessions"
	"github.com/shipyard/shipyard"
	"github.com/shipyard/shipyard/dockerhub"

	"github.com/samalba/dockerclient"
)

const (
	tblNameConfig      = "config"
	tblNameEvents      = "events"
	tblNameAccounts    = "accounts"
	tblNameRoles       = "roles"
	tblNameServiceKeys = "service_keys"
	tblNameExtensions  = "extensions"
	tblNameWebhookKeys = "webhook_keys"
	storeKey           = "shipyard"
	trackerHost        = "http://tracker.shipyard-project.com"
	EngineHealthUp     = "up"
	EngineHealthDown   = "down"
)

var (
	ErrAccountExists          = errors.New("account already exists")
	ErrAccountDoesNotExist    = errors.New("account does not exist")
	ErrRoleDoesNotExist       = errors.New("role does not exist")
	ErrServiceKeyDoesNotExist = errors.New("service key does not exist")
	ErrInvalidAuthToken       = errors.New("invalid auth token")
	ErrExtensionDoesNotExist  = errors.New("extension does not exist")
	ErrWebhookKeyDoesNotExist = errors.New("webhook key does not exist")
	logger                    = logrus.New()
	store                     = sessions.NewCookieStore([]byte(storeKey))
)

type (
	Manager struct {
		address          string
		database         string
		authKey          string
		session          *r.Session
		authenticator    *shipyard.Authenticator
		store            *sessions.CookieStore
		version          string
		disableUsageInfo bool
		client           *dockerclient.DockerClient
		StoreKey         string
	}
)

func NewManager(addr string, database string, authKey string, version string, swarmUrl string, tlsConfig *tls.Config, disableUsageInfo bool) (*Manager, error) {
	session, err := r.Connect(r.ConnectOpts{
		Address:     addr,
		Database:    database,
		AuthKey:     authKey,
		MaxIdle:     10,
		IdleTimeout: time.Second * 30,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("checking database")
	r.DbCreate(database).Run(session)

	client, err := dockerclient.NewDockerClient(swarmUrl, tlsConfig)
	m := &Manager{
		address:          addr,
		database:         database,
		authKey:          authKey,
		session:          session,
		authenticator:    &shipyard.Authenticator{},
		store:            store,
		version:          version,
		disableUsageInfo: disableUsageInfo,
		StoreKey:         storeKey,
		client:           client,
	}
	m.initdb()
	m.init()
	return m, nil
}

func (m *Manager) Store() *sessions.CookieStore {
	return m.store
}

func (m *Manager) initdb() {
	// create tables if needed
	tables := []string{tblNameConfig, tblNameEvents, tblNameAccounts, tblNameRoles, tblNameServiceKeys, tblNameExtensions, tblNameWebhookKeys}
	for _, tbl := range tables {
		_, err := r.Table(tbl).Run(m.session)
		if err != nil {
			if _, err := r.Db(m.database).TableCreate(tbl).Run(m.session); err != nil {
				logger.Fatalf("error creating table: %s", err)
			}
		}
	}
}

func (m *Manager) init() {
	// anonymous usage info
	go m.usageReport()
}

func (m *Manager) usageReport() {
	if m.disableUsageInfo {
		return
	}
	m.uploadUsage()
	t := time.NewTicker(1 * time.Hour).C
	for {
		select {
		case <-t:
			go m.uploadUsage()
		}
	}
}

func (m *Manager) uploadUsage() {
	id := "anon"
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Name != "lo" {
				hw := iface.HardwareAddr.String()
				id = strings.Replace(hw, ":", "", -1)
				break
			}
		}
	}
	info, err := m.client.Info()
	if err != nil {
		logger.Warnf("error getting info: %s", err)
		return
	}
	usage := &shipyard.Usage{
		ID:              id,
		Version:         m.version,
		NumOfImages:     info.Images,
		NumOfContainers: info.Containers,
		TotalCpus:       info.NCPU,
		TotalMemory:     info.MemTotal,
	}
	b, err := json.Marshal(usage)
	if err != nil {
		logger.Warnf("error serializing usage info: %s", err)
	}
	buf := bytes.NewBuffer(b)
	if _, err := http.Post(fmt.Sprintf("%s/update", trackerHost), "application/json", buf); err != nil {
		logger.Warnf("error sending usage info: %s", err)
	}
}

func (m *Manager) Container(id string) (*dockerclient.ContainerInfo, error) {
	containers, err := m.client.ListContainers(true, false, "")
	if err != nil {
		return nil, err
	}
	for _, cnt := range containers {
		if strings.HasPrefix(cnt.Id, id) {
			info, err := m.client.InspectContainer(cnt.Id)
			if err != nil {
				return nil, err
			}
			return info, nil
		}
	}
	return nil, nil
}

func (m *Manager) Logs(id string, options *dockerclient.LogOptions) (io.ReadCloser, error) {
	return m.client.ContainerLogs(id, options)
}

func (m *Manager) Restart(id string) error {
	return m.client.RestartContainer(id, 10)
}

func (m *Manager) Containers(all bool) ([]dockerclient.Container, error) {
	return m.client.ListContainers(all, false, "")
}

func (m *Manager) ContainersByImage(name string, all bool) ([]*dockerclient.ContainerInfo, error) {
	allContainers, err := m.Containers(all)
	if err != nil {
		return nil, err
	}
	imageContainers := []*dockerclient.ContainerInfo{}
	for _, c := range allContainers {
		if strings.Index(c.Image, name) > -1 {
			info, err := m.client.InspectContainer(c.Id)
			if err != nil {
				return nil, err
			}
			imageContainers = append(imageContainers, info)
		}
	}
	return imageContainers, nil
}

func (m *Manager) IdenticalContainers(container *dockerclient.ContainerInfo, all bool) ([]*dockerclient.ContainerInfo, error) {
	containers := []*dockerclient.ContainerInfo{}
	imageContainers, err := m.ContainersByImage(container.Image, all)
	if err != nil {
		return nil, err
	}
	for _, c := range imageContainers {
		args := len(c.Args)
		origArgs := len(container.Args)
		if c.Config.Memory == container.Config.Memory && args == origArgs {
			containers = append(containers, c)
		}
	}
	return containers, nil
}

func (m *Manager) ClusterInfo() (*shipyard.ClusterInfo, error) {
	info, err := m.client.Info()
	if err != nil {
		return nil, err
	}
	clusterInfo := &shipyard.ClusterInfo{
		Cpus:           info.NCPU,
		Memory:         info.MemTotal,
		ContainerCount: info.Containers,
		ImageCount:     info.Images,
		Version:        m.version,
	}
	return clusterInfo, nil
}

func (m *Manager) Stop(id string) error {
	return m.client.StopContainer(id, 10)
}

func (m *Manager) Destroy(id string) error {
	if err := m.client.KillContainer(id, "kill"); err != nil {
		return err
	}
	if err := m.client.RemoveContainer(id, true); err != nil {
		return err
	}
	return nil
}

func (m *Manager) SaveServiceKey(key *shipyard.ServiceKey) error {
	if _, err := r.Table(tblNameServiceKeys).Insert(key).RunWrite(m.session); err != nil {
		return err
	}
	m.init()
	evt := &shipyard.Event{
		Type:    "add-service-key",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("description=%s", key.Description),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) RemoveServiceKey(key string) error {
	k, err := m.ServiceKey(key)
	if err != nil {
		return err
	}
	evt := &shipyard.Event{
		Type:    "remove-service-key",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("description=%s", k.Description),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	if _, err := r.Table(tblNameServiceKeys).Filter(map[string]string{"key": key}).Delete().RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m *Manager) SaveEvent(event *shipyard.Event) error {
	if _, err := r.Table(tblNameEvents).Insert(event).RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Events(limit int) ([]*shipyard.Event, error) {
	t := r.Table(tblNameEvents).OrderBy(r.Desc("Time"))
	if limit > -1 {
		t.Limit(limit)
	}
	res, err := t.Run(m.session)
	if err != nil {
		return nil, err
	}
	events := []*shipyard.Event{}
	if err := res.All(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func (m *Manager) PurgeEvents() error {
	if _, err := r.Table(tblNameEvents).Delete().RunWrite(m.session); err != nil {
		return err
	}
	return nil
}

func (m *Manager) ServiceKey(key string) (*shipyard.ServiceKey, error) {
	res, err := r.Table(tblNameServiceKeys).Filter(map[string]string{"key": key}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrServiceKeyDoesNotExist
	}
	var k *shipyard.ServiceKey
	if err := res.One(&k); err != nil {
		return nil, err
	}
	return k, nil
}

func (m *Manager) ServiceKeys() ([]*shipyard.ServiceKey, error) {
	res, err := r.Table(tblNameServiceKeys).Run(m.session)
	if err != nil {
		return nil, err
	}
	keys := []*shipyard.ServiceKey{}
	if err := res.All(&keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (m *Manager) Accounts() ([]*shipyard.Account, error) {
	res, err := r.Table(tblNameAccounts).OrderBy(r.Asc("username")).Run(m.session)
	if err != nil {
		return nil, err
	}
	accounts := []*shipyard.Account{}
	if err := res.All(&accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}

func (m *Manager) Account(username string) (*shipyard.Account, error) {
	res, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrAccountDoesNotExist
	}
	var account *shipyard.Account
	if err := res.One(&account); err != nil {
		return nil, err
	}
	return account, nil
}

func (m *Manager) SaveAccount(account *shipyard.Account) error {
	pass := account.Password
	hash, err := m.authenticator.Hash(pass)
	if err != nil {
		return err
	}
	// check if exists; if so, update
	acct, err := m.Account(account.Username)
	if err != nil && err != ErrAccountDoesNotExist {
		return err
	}
	account.Password = hash
	if acct != nil {
		if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": account.Username}).Update(map[string]string{"password": hash}).RunWrite(m.session); err != nil {
			return err
		}
		return nil
	}
	if _, err := r.Table(tblNameAccounts).Insert(account).RunWrite(m.session); err != nil {
		return err
	}
	evt := &shipyard.Event{
		Type:    "add-account",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("name=%s", account.Username),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) DeleteAccount(account *shipyard.Account) error {
	res, err := r.Table(tblNameAccounts).Filter(map[string]string{"id": account.ID}).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrAccountDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-account",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("name=%s", account.Username),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Roles() ([]*shipyard.Role, error) {
	res, err := r.Table(tblNameRoles).OrderBy(r.Asc("name")).Run(m.session)
	if err != nil {
		return nil, err
	}
	roles := []*shipyard.Role{}
	if err := res.All(&roles); err != nil {
		return nil, err
	}
	return roles, nil
}

func (m *Manager) Role(name string) (*shipyard.Role, error) {
	res, err := r.Table(tblNameRoles).Filter(map[string]string{"name": name}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrRoleDoesNotExist
	}
	var role *shipyard.Role
	if err := res.One(&role); err != nil {
		return nil, err
	}
	return role, nil
}

func (m *Manager) SaveRole(role *shipyard.Role) error {
	if _, err := r.Table(tblNameRoles).Insert(role).RunWrite(m.session); err != nil {
		return err
	}
	evt := &shipyard.Event{
		Type:    "add-role",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("name=%s", role.Name),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) DeleteRole(role *shipyard.Role) error {
	res, err := r.Table(tblNameRoles).Get(role.ID).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrRoleDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-role",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("name=%s", role.Name),
		Tags:    []string{"cluster", "security"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Authenticate(username, password string) bool {
	acct, err := m.Account(username)
	if err != nil {
		logger.Error(err)
		return false
	}
	return m.authenticator.Authenticate(password, acct.Password)
}

func (m *Manager) NewAuthToken(username string, userAgent string) (*shipyard.AuthToken, error) {
	tk, err := m.authenticator.GenerateToken()
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	acct, err := m.Account(username)
	if err != nil {
		return nil, err
	}
	token := &shipyard.AuthToken{}
	tokens := acct.Tokens
	found := false
	for _, t := range tokens {
		if t.UserAgent == userAgent {
			found = true
			t.Token = tk
			token = t
			break
		}
	}
	if !found {
		token = &shipyard.AuthToken{
			UserAgent: userAgent,
			Token:     tk,
		}
		tokens = append(tokens, token)
	}
	// delete token
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Filter(r.Row.Field("user_agent").Eq(userAgent)).Delete().Run(m.session); err != nil {
		return nil, err
	}
	// add
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Update(map[string]interface{}{"tokens": tokens}).RunWrite(m.session); err != nil {
		return nil, err
	}
	return token, nil
}

func (m *Manager) VerifyAuthToken(username, token string) error {
	acct, err := m.Account(username)
	if err != nil {
		return err
	}
	found := false
	for _, t := range acct.Tokens {
		if token == t.Token {
			found = true
			break
		}
	}
	if !found {
		return ErrInvalidAuthToken
	}
	return nil
}

func (m *Manager) VerifyServiceKey(key string) error {
	if _, err := m.ServiceKey(key); err != nil {
		return err
	}
	return nil
}

func (m *Manager) NewServiceKey(description string) (*shipyard.ServiceKey, error) {
	k, err := m.authenticator.GenerateToken()
	if err != nil {
		return nil, err
	}
	key := &shipyard.ServiceKey{
		Key:         k[24:],
		Description: description,
	}
	if err := m.SaveServiceKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func (m *Manager) ChangePassword(username, password string) error {
	hash, err := m.authenticator.Hash(password)
	if err != nil {
		return err
	}
	if _, err := r.Table(tblNameAccounts).Filter(map[string]string{"username": username}).Update(map[string]string{"password": hash}).Run(m.session); err != nil {
		return err
	}
	return nil
}

func (m *Manager) RedeployContainers(image string) error {
	var cfg *dockerclient.ContainerConfig
	containers, err := m.Containers(false)
	if err != nil {
		return err
	}
	deployed := false
	for _, c := range containers {
		if strings.Index(c.Image, image) > -1 {
			info, err := m.client.InspectContainer(c.Id)
			if err != nil {
				return err
			}
			cfg = info.Config
			logger.Infof("pulling latest image for %s", image)
			if err := m.client.PullImage(image, nil); err != nil {
				return err
			}
			m.Destroy(c.Id)

			containerId, err := m.client.CreateContainer(cfg, "")
			if err != nil {
				return err
			}

			if err := m.client.StartContainer(containerId, info.HostConfig); err != nil {
				return err
			}
			deployed = true
			logger.Infof("deployed updated container %s via webhook for %s", containerId[:8], image)
		}
	}
	if deployed {
		evt := &shipyard.Event{
			Type:    "deploy",
			Message: fmt.Sprintf("%s deployed", image),
			Time:    time.Now().Unix(),
			Tags:    []string{"deploy"},
		}
		if err := m.SaveEvent(evt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) WebhookKeys() ([]*dockerhub.WebhookKey, error) {
	res, err := r.Table(tblNameWebhookKeys).OrderBy(r.Asc("image")).Run(m.session)
	if err != nil {
		return nil, err
	}
	keys := []*dockerhub.WebhookKey{}
	if err := res.All(&keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (m *Manager) NewWebhookKey(image string) (*dockerhub.WebhookKey, error) {
	k := generateId(16)
	key := &dockerhub.WebhookKey{
		Key:   k,
		Image: image,
	}
	if err := m.SaveWebhookKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

func (m *Manager) WebhookKey(key string) (*dockerhub.WebhookKey, error) {
	res, err := r.Table(tblNameWebhookKeys).Filter(map[string]string{"key": key}).Run(m.session)
	if err != nil {
		return nil, err

	}
	if res.IsNil() {
		return nil, ErrWebhookKeyDoesNotExist
	}
	var k *dockerhub.WebhookKey
	if err := res.One(&k); err != nil {
		return nil, err
	}
	return k, nil
}

func (m *Manager) SaveWebhookKey(key *dockerhub.WebhookKey) error {
	if _, err := r.Table(tblNameWebhookKeys).Insert(key).RunWrite(m.session); err != nil {
		return err
	}
	evt := &shipyard.Event{
		Type:    "add-webhook-key",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("image=%s", key.Image),
		Tags:    []string{"docker", "webhook"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) DeleteWebhookKey(id string) error {
	key, err := m.WebhookKey(id)
	if err != nil {
		return err
	}
	res, err := r.Table(tblNameWebhookKeys).Get(key.ID).Delete().Run(m.session)
	if err != nil {
		return err
	}
	if res.IsNil() {
		return ErrWebhookKeyDoesNotExist
	}
	evt := &shipyard.Event{
		Type:    "delete-webhook-key",
		Time:    time.Now().Unix(),
		Message: fmt.Sprintf("image=%s key=%s", key.Image, key.Key),
		Tags:    []string{"docker", "webhook"},
	}
	if err := m.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Run(config *dockerclient.ContainerConfig, count int, pull bool) ([]string, error) {
	launched := []string{}

	var wg sync.WaitGroup
	wg.Add(count)
	var runErr error
	for i := 0; i < count; i++ {
		go func(wg *sync.WaitGroup) {
			if pull {
				if err := m.client.PullImage(config.Image, nil); err != nil {
					runErr = err
					return
				}
			}
			containerId, err := m.client.CreateContainer(config, "")
			if err != nil {
				runErr = err
				return
			}
			if err := m.client.StartContainer(containerId, &config.HostConfig); err != nil {
				runErr = err
				return
			}
			launched = append(launched, containerId)
			wg.Done()
		}(&wg)
	}
	wg.Wait()
	return launched, runErr
}

func (m *Manager) Scale(container *dockerclient.ContainerInfo, count int) error {
	info, err := m.client.InspectContainer(container.Id)
	if err != nil {
		return err
	}
	imageContainers, err := m.IdenticalContainers(info, true)
	if err != nil {
		return err
	}
	containerCount := len(imageContainers)
	// check which way we need to scale
	if containerCount > count { // down
		numKill := containerCount - count
		delContainers := imageContainers[0:numKill]
		for _, c := range delContainers {
			if err := m.Destroy(c.Id); err != nil {
				return err
			}
		}
	} else if containerCount < count { // up
		numAdd := count - containerCount
		// reset hostname
		container.Config.Hostname = ""
		if _, err := m.Run(container.Config, numAdd, false); err != nil {
			return err
		}
	} else { // none
		logger.Info("no need to scale")
	}
	return nil
}
