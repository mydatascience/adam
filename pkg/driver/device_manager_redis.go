// Copyright (c) 2020 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"github.com/golang/protobuf/jsonpb"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/golang/protobuf/proto"
	ax "github.com/lf-edge/adam/pkg/x509"
	"github.com/lf-edge/eve/api/go/config"
	"github.com/lf-edge/eve/api/go/info"
	"github.com/lf-edge/eve/api/go/logs"
	"github.com/lf-edge/eve/api/go/metrics"
	uuid "github.com/satori/go.uuid"
	"github.com/vmihailenco/msgpack/v4"
)

const (
	// Our current schema for Redis database is that aside from logs, info and metrics
	// everything else is kept in Redis hashes with the following mapping:
	onboardCertsHash = "ONBOARD_CERTS" // CN -> string (certificate PEM)
	onboardSerialsHash = "ONBOARD_SERIALS" // CN -> []string (list of serial #s)
	deviceSerialsHash = "DEVICE_SERIALS" // UUID -> string (single serial #)
	deviceOnboardCertsHash = "DEVICE_ONBOARD_CERTS" // UUID -> string (certificate PEM)
	deviceCertsHash = "DEVICE_CERTS"  // UUID -> string (certificate PEM)
	deviceConfigsHash = "DEVICE_CONFIGS" // UUID -> json (EVE config json representation)

	// Logs, info and metrics are managed by Redis streams named after device UUID as in:
	//    LOGS_EVE_<UUID>
	//    INFO_EVE_<UUID>
	//    METRICS_EVE_<UUID>
	// with each stream element having a single key pair:
	//   "object" -> msgpack serialized object
	// see MkStreamEntry() for details
	deviceLogsStream = "LOGS_EVE_"
	deviceInfoSteram = "INFO_EVE_"
	deviceMetricsStream = "METRICS_EVE_"
)

// DeviceManagerRedis implementation of DeviceManager interface with a Redis DB as the backing store
type DeviceManagerRedis struct {
	client       *redis.Client
	databaseNet  string
	databaseURL  string
	databaseID   int
	cacheTimeout int
	lastUpdate   time.Time
	// these are for caching only
	onboardCerts map[string]map[string]bool
	deviceCerts  map[string]uuid.UUID
	devices      map[uuid.UUID]deviceStorage
}

// Name return name
func (d *DeviceManagerRedis) Name() string {
	return "redis"
}

// Database return database hostname and port
func (d *DeviceManagerRedis) Database() string {
	return d.databaseURL
}

// MaxLogSize return the default maximum log size in bytes for this device manager
func (d *DeviceManagerRedis) MaxLogSize() int {
	return maxLogSizeFile
}

// MaxInfoSize return the default maximum info size in bytes for this device manager
func (d *DeviceManagerRedis) MaxInfoSize() int {
	return maxInfoSizeFile
}

// MaxMetricSize return the maximum metrics size in bytes for this device manager
func (d *DeviceManagerRedis) MaxMetricSize() int {
	return maxMetricSizeFile
}

// Init check if a URL is valid and initialize
func (d *DeviceManagerRedis) Init(s string, maxLogSize, maxInfoSize, maxMetricSize int) (bool, error) {
	URL, err := url.Parse(s)
	if err != nil || URL.Scheme != "redis" {
		return false, err
	}

	d.databaseNet = "tcp"

	if URL.Host != "" {
		d.databaseURL = URL.Host
	} else {
		d.databaseURL = "localhost:6379"
	}
	if URL.Path != "" {
		if d.databaseID, err = strconv.Atoi(strings.Trim(URL.Path, "/")); err != nil {
			return false, err
		}
	} else {
		d.databaseID = 0
	}

	d.client = redis.NewClient(&redis.Options{
		Network:  d.databaseNet,
		Addr:     d.databaseURL,
		Password: URL.User.Username(), // yes, I know!
		DB:       d.databaseID,
	})

	return true, nil
}

// SetCacheTimeout set the timeout for refreshing the cache, unused in memory
func (d *DeviceManagerRedis) SetCacheTimeout(timeout int) {
	d.cacheTimeout = timeout
}

// OnboardCheck see if a particular certificate and serial combination is valid
func (d *DeviceManagerRedis) OnboardCheck(cert *x509.Certificate, serial string) error {
	// do not accept a nil certificate
	if cert == nil {
		return fmt.Errorf("invalid nil certificate")
	}
	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}

	if err := d.checkValidOnboardSerial(cert, serial); err != nil {
		return err
	}
	if d.getOnboardSerialDevice(cert, serial) != nil {
		return &UsedSerialError{err: fmt.Sprintf("serial already used for onboarding certificate: %s", serial)}
	}
	return nil
}

// OnboardGet get the onboard cert and its serials based on Common Name
func (d *DeviceManagerRedis) OnboardGet(cn string) (*x509.Certificate, []string, error) {
	if cn == "" {
		return nil, nil, fmt.Errorf("empty cn")
	}

	cert, err := d.readCert(onboardCertsHash, cn)
	if err != nil {
		return nil, nil, err
	}

	s, err := d.client.HGet(onboardSerialsHash, cn).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("error reading onboard serials for %s: %v", cn, err)
	}
    var serials []string
	if err = msgpack.Unmarshal([]byte(s), &serials); err != nil {
		return nil, nil, fmt.Errorf("error decoding onboard serials for %s %v (%s)", cn, err, s)
	}
	return cert, serials, nil
}

// OnboardList list all of the known Common Names for onboard
func (d *DeviceManagerRedis) OnboardList() ([]string, error) {
	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}
	cns := make([]string, 0)
	for certStr := range d.onboardCerts {
		certRaw := []byte(certStr)
		cert, err := x509.ParseCertificate(certRaw)
		if err != nil {
			return nil, fmt.Errorf("unable to parse certificate: %v", err)
		}
		cns = append(cns, cert.Subject.CommonName)
	}
	return cns, nil
}

// OnboardRemove remove an onboard certificate based on Common Name
func (d *DeviceManagerRedis) OnboardRemove(cn string) (result error) {
	result = d.transactionDrop([][]string{{onboardSerialsHash, cn},
		                                  {onboardCertsHash, cn}})
	if result == nil {
		result = d.refreshCache()
	}
	return
}

// OnboardClear remove all onboarding certs
func (d *DeviceManagerRedis) OnboardClear() error {
	if err := d.transactionDrop([][]string{{onboardCertsHash}, {onboardSerialsHash}}); err != nil {
		return fmt.Errorf("unable to remove the onboarding certificates/serials: %v", err)
	}

	d.onboardCerts = map[string]map[string]bool{}
	return nil
}

// DeviceCheckCert see if a particular certificate is a valid registered device certificate
func (d *DeviceManagerRedis) DeviceCheckCert(cert *x509.Certificate) (*uuid.UUID, error) {
	if cert == nil {
		return nil, fmt.Errorf("invalid nil certificate")
	}
	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}
	certStr := string(cert.Raw)
	if u, ok := d.deviceCerts[certStr]; ok {
		return &u, nil
	}
	return nil, nil
}

// DeviceRemove remove a device
func (d *DeviceManagerRedis) DeviceRemove(u *uuid.UUID) error {
	k := u.String()
	err := d.transactionDrop([][]string{
		{deviceCertsHash, k},
		{deviceConfigsHash, k},
		{deviceOnboardCertsHash, k},
		{deviceSerialsHash, k},
		{deviceInfoSteram + k},
		{deviceLogsStream + k},
		{deviceMetricsStream + k}})

	if err != nil {
		return fmt.Errorf("unable to remove the device %s %v", k, err)
	}
	// refresh the cache
	err = d.refreshCache()
	if err != nil {
		return fmt.Errorf("unable to refresh device cache: %v", err)
	}
	return nil
}

// DeviceClear remove all devices
func (d *DeviceManagerRedis) DeviceClear() error {
	streams := [][]string{
		{deviceConfigsHash},
		{deviceSerialsHash},
		{deviceCertsHash},
		{deviceOnboardCertsHash}}

	for u, _ := range d.devices {
		streams = append(streams,
			[]string{deviceMetricsStream + u.String()},
			[]string{deviceLogsStream + u.String()},
			[]string{deviceInfoSteram + u.String()})

	}

	err := d.transactionDrop(streams)

	if err != nil {
		return fmt.Errorf("unable to remove all devices %v", err)
	}

	d.deviceCerts = map[string]uuid.UUID{}
	d.devices = map[uuid.UUID]deviceStorage{}
	return nil
}

// DeviceGet get an individual device by UUID
func (d *DeviceManagerRedis) DeviceGet(u *uuid.UUID) (*x509.Certificate, *x509.Certificate, string, error) {
	if u == nil {
		return nil, nil, "", fmt.Errorf("empty UUID")
	}

	// first lets get the device certificate
	cert, err := d.readCert(deviceCertsHash, u.String())
	if err != nil {
		return nil, nil, "", err
	}

	// now lets get the device onboarding certificate
	onboard, err := d.readCert(deviceOnboardCertsHash, u.String())
	if err != nil {
		return nil, nil, "", err
	}

	serial, err := d.client.HGet(deviceSerialsHash, u.String()).Result()
	// somehow device serials are best effort
	return cert, onboard, serial, nil
}

// DeviceList list all of the known UUIDs for devices
func (d *DeviceManagerRedis) DeviceList() ([]*uuid.UUID, error) {
	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(d.devices))
	for u := range d.devices {
		ids = append(ids, u)
	}
	pids := make([]*uuid.UUID, 0, len(ids))
	for i := range ids {
		pids = append(pids, &ids[i])
	}
	return pids, nil
}

// DeviceRegister register a new device cert
func (d *DeviceManagerRedis) DeviceRegister(cert, onboard *x509.Certificate, serial string) (*uuid.UUID, error) {
	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}
	// check if it already exists - this also checks for nil cert
	u, err := d.DeviceCheckCert(cert)
	if err != nil {
		return nil, err
	}
	// if we found a uuid, then it already exists
	if u != nil {
		return nil, fmt.Errorf("device already registered")
	}
	// generate a new uuid
	unew, err := uuid.NewV4()
	if err != nil {
		return nil, fmt.Errorf("error generating uuid for device: %v", err)
	}

	// save the device certificate
	err = d.writeCert(cert.Raw, deviceCertsHash, unew.String(), true)
	if err != nil {
		return nil, err
	}

	// save the onboard certificate and serial, if provided
	if onboard != nil {
		err = d.writeCert(onboard.Raw, deviceOnboardCertsHash, unew.String(), true)
		if err != nil {
			return nil, err
		}
	}
	if serial != "" {
		if _, err = d.client.HSet(deviceSerialsHash, unew.String(), serial).Result(); err == nil {
			_, err = d.client.Save().Result()
		}
		if err != nil {
			return nil, fmt.Errorf("error saving device serial for %v: %v", unew, err)
		}
	}

	// save the base configuration
	err = d.writeProtobufToJSONMsgPack(unew, deviceConfigsHash, createBaseConfig(unew))
	if err != nil {
		return nil, fmt.Errorf("error saving device config for %v: %v", unew, err)
	}

	// create the necessary Redis streams for this device
	for _, p := range []string{deviceInfoSteram, deviceMetricsStream, deviceLogsStream} {
		stream := p + unew.String()
		_, err := d.client.XAdd(&redis.XAddArgs{
			Stream: stream,
			MaxLenApprox: 10000,
			ID: "*",
			Values: d.mkStreamEntry([]byte("")),
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("error creating stream %s: %v", stream, err)
		}
	}

	// save new one to cache - just the serial and onboard; the rest is on disk
	d.deviceCerts[string(cert.Raw)] = unew
	d.devices[unew] = deviceStorage{
		onboard: onboard,
		serial:  serial,
	}

	return &unew, nil
}

// OnboardRegister register an onboard cert and update its serials
func (d *DeviceManagerRedis) OnboardRegister(cert *x509.Certificate, serial []string) error {
	if cert == nil {
		return fmt.Errorf("empty nil certificate")
	}
	certStr := string(cert.Raw)
	cn := getOnboardCertName(cert.Subject.CommonName)

	if err := d.writeCert(cert.Raw, onboardCertsHash, cn, true); err != nil {
		return err
	}

	v, err := msgpack.Marshal(&serial)
	if err != nil {
		return fmt.Errorf("failed to serialize serials %v: %v", serial, err)
	}

	if _, err = d.client.HSet(onboardSerialsHash, cn, v).Result(); err == nil {
		_, err = d.client.Save().Result()
	}
	if err != nil {
		return fmt.Errorf("failed to save serials %v: %v", serial, err)
	}

	// update the cache
	if d.onboardCerts == nil {
		d.onboardCerts = map[string]map[string]bool{}
	}
	serialList := map[string]bool{}
	for _, s := range serial {
		serialList[s] = true
	}
	d.onboardCerts[certStr] = serialList

	return nil
}

// WriteInfo write an info message
func (d *DeviceManagerRedis) WriteInfo(m *info.ZInfoMsg) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevId)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevId, err)
	}
	// check that the device actually exists
	err = d.writeProtobufToStream(deviceInfoSteram + u.String(), m)
	if err != nil {
		return fmt.Errorf("failed to write info to a stream: %v", err)
	}
	return nil
}

// WriteLogs write a message of logs
func (d *DeviceManagerRedis) WriteLogs(m *logs.LogBundle) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevID)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevID, err)
	}
	// check that the device actually exists
	err = d.writeProtobufToStream(deviceLogsStream + u.String(), m)
	if err != nil {
		return fmt.Errorf("failed to write info to a stream: %v", err)
	}
	return nil
}

// WriteMetrics write a metrics message
func (d *DeviceManagerRedis) WriteMetrics(m *metrics.ZMetricMsg) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevID)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevID, err)
	}
	// check that the device actually exists
	err = d.writeProtobufToStream(deviceMetricsStream + u.String(), m)
	if err != nil {
		return fmt.Errorf("failed to write info to a stream: %v", err)
	}
	return nil
}

// GetConfig retrieve the config for a particular device
func (d *DeviceManagerRedis) GetConfig(u uuid.UUID) (*config.EdgeDevConfig, error) {
	// hold our config
	msg := &config.EdgeDevConfig{}
	b, err := d.client.HGet(deviceConfigsHash, u.String()).Result()
	if err != nil {
		// if config doesn't exist - create an empty one
		msg = createBaseConfig(u)
		v, err := msgpack.Marshal(&msg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshall config %v", err)
		}
		if _, err = d.client.HSet(deviceConfigsHash, u.String(), string(v)).Result(); err == nil {
			_, err = d.client.Save().Result()
		}
		if err != nil {
			return nil, fmt.Errorf("failed to save config for %s: %v", u.String(), err)
		}
	} else {
		err := msgpack.Unmarshal([]byte(b), &msg)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal config %v", err)
		}
	}

	return msg, nil
}

// GetConfigResponse retrieve the config for a particular device
func (d *DeviceManagerRedis) GetConfigResponse(u uuid.UUID) (*config.ConfigResponse, error) {
	// hold our config
	msg := &config.EdgeDevConfig{}

	msg, err := d.GetConfig(u)
	if err != nil {
		return nil, err
	}

	response := &config.ConfigResponse{}

	h := sha256.New()
	computeConfigElementSha(h, msg)
	configHash := h.Sum(nil)

	response.Config = msg
	response.ConfigHash = base64.URLEncoding.EncodeToString(configHash)

	return response, nil
}

// SetConfig set the config for a particular device
func (d *DeviceManagerRedis) SetConfig(u uuid.UUID, m *config.EdgeDevConfig) error {
	// pre-flight checks to bail early
	if m == nil {
		return fmt.Errorf("empty configuration")
	}
	// check for UUID mismatch
	if m.Id == nil || m.Id.Uuid != u.String() {
		return fmt.Errorf("mismatched UUID")
	}

	// refresh certs from Redis, if needed - includes checking if necessary based on timer
	err := d.refreshCache()
	if err != nil {
		return fmt.Errorf("unable to refresh certs from Redis: %v", err)
	}
	// look up the device by uuid
	_, ok := d.devices[u]
	if !ok {
		return fmt.Errorf("unregistered device UUID %s", u.String())
	}

	// save the base configuration
	v, err := msgpack.Marshal(&m)
	if err != nil {
		return fmt.Errorf("failed to marshall config %v", err)
	}
	if _, err = d.client.HSet(deviceConfigsHash, u.String(), string(v)).Result(); err == nil {
		_, err = d.client.Save().Result()
	}
	if err != nil {
		return fmt.Errorf("failed to save config for %s: %v", u.String(), err)
	}
	return nil
}

// GetLogsReader get the logs for a given uuid
func (d *DeviceManagerRedis) GetLogsReader(u uuid.UUID) (io.Reader, error) {
	dr := &RedisStreamReader{
		Client:   d.client,
		Stream:   deviceLogsStream + u.String(),
		LineFeed: true,
	}
	return dr, nil
}

// GetInfoReader get the info for a given uuid
func (d *DeviceManagerRedis) GetInfoReader(u uuid.UUID) (io.Reader, error) {
	dr := &RedisStreamReader{
		Client:   d.client,
		Stream:   deviceInfoSteram + u.String(),
		LineFeed: true,
	}
	return dr, nil
}

// refreshCache refresh cache from disk
func (d *DeviceManagerRedis) refreshCache() error {
	// is it time to update the cache again?
	now := time.Now()
	if now.Sub(d.lastUpdate).Seconds() < float64(d.cacheTimeout) {
		return nil
	}

	// create new vars to hold while we load
	onboardCerts := make(map[string]map[string]bool)
	deviceCerts := make(map[string]uuid.UUID)
	devices := make(map[uuid.UUID]deviceStorage)

	// scan the onboarding certs
	ocerts, err := d.client.HGetAll(onboardCertsHash).Result()
	if err != nil {
		return fmt.Errorf("failed to retrieve onboarding certificated from %s %v", onboardCertsHash, err)
	}

	for u, c := range ocerts {
		certPem, _ := pem.Decode([]byte(c))
		cert, err := x509.ParseCertificate(certPem.Bytes)
		if err != nil {
			return fmt.Errorf("unable to convert data from %s to onboard certificate: %v", c, err)
		}
		certStr := string(cert.Raw)
		onboardCerts[certStr] = make(map[string]bool)

		v, err := d.client.HGet(onboardSerialsHash, u).Result()
		if err != nil {
			return fmt.Errorf("unabled to get a serial for %s: %v", u, err)
		}

		var serials []string
		err = msgpack.Unmarshal([]byte(v), &serials)
		if err != nil {
			return fmt.Errorf("unable to unmarshal onboard serials %s: %v", v, err)
		}
		for _, serial := range serials {
			onboardCerts[certStr][serial] = true
		}
	}
	// replace the existing onboard certificates
	d.onboardCerts = onboardCerts

	// scan the device certs
	dcerts, err := d.client.HGetAll(deviceCertsHash).Result()
	if err != nil {
		return fmt.Errorf("failed to retrieve device certificates from %s %v", deviceCertsHash, err)
	}

	// check each Redis hash to see if it is valid
	for k, c := range dcerts {
		// convert the path name to a UUID
		u, err := uuid.FromString(k)
		if err != nil {
			return fmt.Errorf("unable to convert device uuid from Redis hash name %s: %v", u, err)
		}

		// load the device certificate
		certPem, _ := pem.Decode([]byte(c))
		cert, err := x509.ParseCertificate(certPem.Bytes)
		if err != nil {
			return fmt.Errorf("unable to convert data from file %s to device certificate: %v", c, err)
		}
		certStr := string(cert.Raw)
		deviceCerts[certStr] = u
		devices[u] = deviceStorage{}
	}
	// replace the existing device certificates
	d.deviceCerts = deviceCerts

	// scan the device onboarding certs
	docerts, err := d.client.HGetAll(deviceOnboardCertsHash).Result()
	if err != nil {
		return fmt.Errorf("failed to retrieve device certificates from %s %v", deviceCertsHash, err)
	}

	// check each Redis hash to see if it is valid
	for k, b := range docerts {
		// convert the path name to a UUID
		u, err := uuid.FromString(k)
		if err != nil {
			return fmt.Errorf("unable to convert device uuid from Redis hash name %s: %v", u, err)
		}

		certPem, _ := pem.Decode([]byte(b))
		cert, err := x509.ParseCertificate(certPem.Bytes)
		if err != nil {
			return fmt.Errorf("unable to convert data from file %s to device onboard certificate: %v", b, err)
		}
		if _, present := devices[u]; !present {
			devices[u] = deviceStorage{}
		}
		devItem := devices[u]
		devItem.onboard = cert
		devices[u] = devItem
	}

	// scan the device onboarding certs
	dserials, err := d.client.HGetAll(deviceSerialsHash).Result()
	if err != nil {
		return fmt.Errorf("failed to retrieve device certificates from %s %v", deviceCertsHash, err)
	}

	for k, s := range dserials {
		// convert the path name to a UUID
		u, err := uuid.FromString(k)
		if err != nil {
			return fmt.Errorf("unable to convert device uuid from Redis hash name %s: %v", u, err)
		}
		if _, present := devices[u]; !present {
			devices[u] = deviceStorage{}
		}
		devItem := devices[u]
		devItem.serial = s
		devices[u] = devItem
	}
	// replace the existing device cache
	d.devices = devices

	// mark the time we updated
	d.lastUpdate = now
	return nil
}

// writeProtobufToJSONMsgPack write a protobuf to a named hash in Redis
func (d *DeviceManagerRedis) writeProtobufToJSONMsgPack(u uuid.UUID, hash string, msg proto.Message) error {
	s, err := msgpack.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("can't marshal proto message %v", err)
	}

	if _, err = d.client.HSet(hash, u.String(), s).Result(); err == nil {
		_, err = d.client.Save().Result()
	}
	if err != nil {
		return fmt.Errorf("can't save proto message for %s in %s: %v", u.String(), hash, err)
	}
	return nil
}

// checkValidOnboardSerial see if a particular certificate+serial combinaton is valid
// does **not** check if it has been used
func (d *DeviceManagerRedis) checkValidOnboardSerial(cert *x509.Certificate, serial string) error {
	certStr := string(cert.Raw)
	if c, ok := d.onboardCerts[certStr]; ok {
		// accept the specific serial or the wildcard
		if _, ok := c[serial]; ok {
			return nil
		}
		if _, ok := c["*"]; ok {
			return nil
		}
		return &InvalidSerialError{err: fmt.Sprintf("unknown serial: %s", serial)}
	}
	return &InvalidCertError{err: "unknown onboarding certificate"}
}

// getOnboardSerialDevice see if a particular certificate+serial combinaton has been used and get its device uuid
func (d *DeviceManagerRedis) getOnboardSerialDevice(cert *x509.Certificate, serial string) *uuid.UUID {
	certStr := string(cert.Raw)
	for uid, dev := range d.devices {
		dCertStr := string(dev.onboard.Raw)
		if dCertStr == certStr && serial == dev.serial {
			return &uid
		}
	}
	return nil
}

func (d *DeviceManagerRedis) transactionDrop(keys [][]string) (result error) {
	// transactionality of this function is currently a lie: later on we
	// will turn it into one, but for now lets just hope that we never
	// get into an inconsistent state between all these objects that need
	// to be droppped
	for _, k := range keys {
		switch len(k) {
		case 1:
			if i, err := d.client.Del(k[0]).Result(); i != 1 || err != nil {
				result = fmt.Errorf("couldn't drop %s with error %d/%v (previous error in transaction %v)",
					k[0], i, err, result)
			}
		case 2:
			if i, err := d.client.HDel(k[0], k[1]).Result(); i != 1 || err != nil {
				result = fmt.Errorf("couldn't drop %s[%s] with error %d/%v (previous error in transaction %v)",
					k[0], k[1], i, err, result)
			}
		default:
			panic("transactionDrop should never be called with keys that are less than 1 or more than 2 elements")
		}
	}
	return
}

func (d *DeviceManagerRedis) readCert(hash string, key string) (*x509.Certificate, error) {
	v, err := d.client.HGet(hash, key).Result()
	if err != nil {
		return nil, fmt.Errorf("error reading certificate for %s from hash %s: %v", key, hash, err)
	}

	if cert, err := ax.ParseCert([]byte(v)); err != nil {
		return nil, fmt.Errorf("error decoding onboard certificate for %s from hash %s: %v (%s)", key, hash, err, v)
	} else {
		return cert, nil
	}
}

// WriteCert write cert bytes to a path, after pem encoding them. Do not overwrite unless force is true.
func (d *DeviceManagerRedis) writeCert(cert []byte, hash string, uuid string, force bool) error {
	// make sure we have the paths we need, and that they are not already taken, unless we were told to force
	if hash == "" {
		return fmt.Errorf("certPath must not be empty")
	}

	if _, err := d.client.HGet(hash, uuid).Result(); err == nil && !force {
		return fmt.Errorf("certificate for %s already exists in %s", uuid, hash)
	}
	certPem := ax.PemEncodeCert(cert)
	if b, err := d.client.HSet(hash, uuid, certPem).Result(); err != nil || (!b && !force) {
		return fmt.Errorf("failed to write certificate for %s: %v", uuid, err)
	}
	if _, err := d.client.Save().Result(); err != nil {
		return fmt.Errorf("failed to write certificate for %s: %v", uuid, err)
	}

	return nil
}

func (d *DeviceManagerRedis) writeProtobufToStream(stream string, msg proto.Message) error {
	var buf bytes.Buffer
	mler := jsonpb.Marshaler{}
	err := mler.Marshal(&buf, msg)
	if err != nil {
		return fmt.Errorf("failed to marshal protobuf message %v: %v", msg, err)
	}

	// XXX: lets see if this blocks
	_, err = d.client.XAdd(&redis.XAddArgs{
		Stream: stream,
		ID: "*",
		Values: d.mkStreamEntry(buf.Bytes()),
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to put protobuf message %v into a stream %s", msg, stream)
	}
	return nil
}

func (d *DeviceManagerRedis) mkStreamEntry(body []byte) map[string]interface{} {
	return map[string]interface{}{"version": "1", "object": string(body)}
}
