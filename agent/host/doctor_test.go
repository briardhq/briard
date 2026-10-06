package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"briard.io/agent/reportcard"
	"briard.io/shared/api"
	"briard.io/shared/model"
)

// servingLone is a healthy lone node: every check it produces must pass.
func servingLone() doctorFacts {
	return doctorFacts{
		Parent: "eth0", ParentUp: true,
		DataDisk: "/dev/sdb", DiskFound: true,
		Cluster:   model.Cluster{QuorumState: model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true}},
		Probe:     "http://192.168.1.50/healthz",
		Healthy:   true,
		FlockName: "brave-elf", Published: "brave-elf",
		VolAsked: true, VolFree: 3 << 30, VolTot: 4 << 30,
		CertAsked: true,
		Now:       time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
	}
}

func checkNamed(t *testing.T, cs []reportcard.Check, name string) reportcard.Check {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, cs)
	return reportcard.Check{}
}

func TestDoctorHealthyLoneNodeAllPass(t *testing.T) {
	cs := judgeDoctor(servingLone())
	want := []string{"network", "data-disk", "guest", "role", "address", "name", "volume", "cert"}
	if len(cs) != len(want) {
		t.Fatalf("checks = %+v, want exactly %v (a lone node has no replication check)", cs, want)
	}
	for i, c := range cs {
		if c.Name != want[i] || c.Status != reportcard.Pass {
			t.Errorf("check %d = %s/%s (%s), want %s/pass", i, c.Name, c.Status, c.Detail, want[i])
		}
	}
}

// Each fault a household can feel names itself as FAIL, with something to do.
func TestDoctorFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*doctorFacts)
		check  string
	}{
		{"parent gone", func(f *doctorFacts) { f.ParentUp = false }, "network"},
		{"data disk missing", func(f *doctorFacts) { f.DiskFound = false }, "data-disk"},
		{"guest silent", func(f *doctorFacts) { f.GuestErr = errors.New("timeout") }, "guest"},
		{"primary without quorum", func(f *doctorFacts) { f.Peers = 1; f.Cluster.Quorate = false }, "role"},
		{"nobody holds the home", func(f *doctorFacts) { f.Cluster.Primary = false }, "role"},
		{"serving with no address", func(f *doctorFacts) { f.Probe = "" }, "address"},
		{"front door silent", func(f *doctorFacts) { f.Healthy = false }, "address"},
		{"standby holding the address", func(f *doctorFacts) {
			f.Peers = 1
			f.Cluster = model.Cluster{QuorumState: model.QuorumState{Quorate: true, Connected: 1, Diskful: true, UpToDate: true},
				Peers: []model.PeerState{{Connected: true, Diskful: true, UpToDate: true}}}
			f.HeldVIP = "192.168.1.50/24"
		}, "address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := servingLone()
			tc.mutate(&f)
			c := checkNamed(t, judgeDoctor(f), tc.check)
			if c.Status != reportcard.Refuse || c.Fix == "" {
				t.Errorf("%s = %+v, want a FAIL with a fix", tc.check, c)
			}
		})
	}
}

// A silent guest ends the report there: nothing after it would be the guest's answer.
func TestDoctorStopsAtASilentGuest(t *testing.T) {
	f := servingLone()
	f.GuestErr = errors.New("timeout")
	cs := judgeDoctor(f)
	if last := cs[len(cs)-1]; last.Name != "guest" {
		t.Errorf("last check = %q, want the report to end at the guest: %+v", last.Name, cs)
	}
}

// A standby beside a peer that can take over is the system working, and must read so.
func TestDoctorStandbyPasses(t *testing.T) {
	f := servingLone()
	f.Peers = 1
	f.Cluster = model.Cluster{QuorumState: model.QuorumState{Quorate: true, Connected: 1, Diskful: true, UpToDate: true},
		Peers: []model.PeerState{{Connected: true, Diskful: true, UpToDate: true}}}
	f.Probe, f.Healthy = "", false
	cs := judgeDoctor(f)
	for _, name := range []string{"role", "replication", "address"} {
		if c := checkNamed(t, cs, name); c.Status != reportcard.Pass {
			t.Errorf("standby %s = %+v, want pass", name, c)
		}
	}
	for _, c := range cs {
		if c.Name == "volume" || c.Name == "cert" || c.Name == "name" {
			t.Errorf("a standby cannot read the volume, yet reported %q", c.Name)
		}
	}
}

func TestDoctorWarnings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*doctorFacts)
		check  string
	}{
		{"no peer connected", func(f *doctorFacts) { f.Peers = 1; f.Cluster.Connected = 0 }, "replication"},
		{"copy not up to date", func(f *doctorFacts) { f.Peers = 1; f.Cluster.Connected = 1; f.Cluster.UpToDate = false }, "replication"},
		{"name not published", func(f *doctorFacts) { f.Published = "" }, "name"},
		{"another briard answers briard.local", func(f *doctorFacts) { f.Other = "192.168.1.50" }, "name"},
		{"volume nearly full", func(f *doctorFacts) { f.VolFree = 100 << 20 }, "volume"},
		{"volume unreadable", func(f *doctorFacts) { f.VolErr = errors.New("no") }, "volume"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := servingLone()
			tc.mutate(&f)
			if c := checkNamed(t, judgeDoctor(f), tc.check); c.Status != reportcard.Warn {
				t.Errorf("%s = %+v, want warn", tc.check, c)
			}
		})
	}
}

func testCert(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestDoctorCert(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name string
		pem  string
		err  error
		want reportcard.Status
	}{
		{"none issued", "", nil, reportcard.Pass},
		{"valid", testCert(t, now.Add(-day), now.Add(60*day)), nil, reportcard.Pass},
		{"expiring", testCert(t, now.Add(-day), now.Add(3*day)), nil, reportcard.Warn},
		{"expired", testCert(t, now.Add(-90*day), now.Add(-day)), nil, reportcard.Refuse},
		{"clock behind", testCert(t, now.Add(day), now.Add(90*day)), nil, reportcard.Refuse},
		{"garbage", "not a cert", nil, reportcard.Refuse},
		// Could not read is not "has none": it must not pass.
		{"unreadable", "", errors.New("io"), reportcard.Warn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c := certCheck(tc.pem, tc.err, now); c.Status != tc.want {
				t.Errorf("cert = %+v, want %s", c, tc.want)
			}
		})
	}
}

// certFake adds the cert read to the host tests' guest fake.
type certFake struct {
	fakeStatus
	pem string
}

func (f certFake) ReadCert(context.Context) (string, error) { return f.pem, nil }

// End to end through dispatch on the LOCAL door: the snapshot the report loop uses, the cert read
// on the serving node, and the checks carried home as JSON the CLI decodes.
func TestDoctorThroughDispatch(t *testing.T) {
	now := time.Now()
	r := certFake{
		fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true, Diskful: true, UpToDate: true}, health: true, vip: "192.168.1.50/24"},
		pem:        testCert(t, now.Add(-time.Hour), now.Add(-time.Minute)),
	}
	cfg := Config{VIPDev: "eth2"}
	o := cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: api.DirectiveDoctor}, originLocal, r, nil, nil, nil, nil, nil, func(string, ...any) {})
	if o.State != api.OutcomeDone || o.ID != "d1" {
		t.Fatalf("outcome = %+v", o)
	}
	var cs []reportcard.Check
	if err := json.Unmarshal([]byte(o.Detail), &cs); err != nil {
		t.Fatalf("detail is not the checks: %v: %q", err, o.Detail)
	}
	if c := checkNamed(t, cs, "address"); c.Status != reportcard.Pass || !strings.Contains(c.Detail, "192.168.1.50") {
		t.Errorf("address = %+v, want the front door the snapshot probed", c)
	}
	if c := checkNamed(t, cs, "cert"); c.Status != reportcard.Refuse {
		t.Errorf("cert = %+v, want the expired cert read through the guest to fail", c)
	}
}
