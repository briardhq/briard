package reportcard

import (
	"os"
	"strings"
	"testing"
)

// Each answer smartctl gives, reduced to what is acted on. The two files are real smartctl 7.5
// output of the exact command readSMART runs; the rest are the same shape with the readings
// that matter changed.
func TestParseSMART(t *testing.T) {
	file := func(name string) []byte {
		b, err := os.ReadFile("testdata/smart/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ata := func(passed string, attrs string) []byte {
		return []byte(`{"smart_status":{"passed":` + passed + `},"ata_smart_attributes":{"revision":16,"table":[` + attrs + `]}}`)
	}
	attr := func(id, raw string) string {
		return `{"id":` + id + `,"name":"x","value":100,"worst":100,"thresh":10,"raw":{"value":` + raw + `,"string":"` + raw + `"}}`
	}
	nvme := func(passed string, warn, media string) []byte {
		return []byte(`{"smart_status":{"passed":` + passed + `},"nvme_smart_health_information_log":{"critical_warning":` + warn +
			`,"available_spare":100,"available_spare_threshold":10,"percentage_used":2,"media_errors":` + media + `}}`)
	}
	for _, c := range []struct {
		name    string
		raw     []byte
		read    bool
		failing bool
		errors  int64
		why     string
	}{
		{"nvme healthy (real)", file("nvme-healthy.json"), true, false, 0, ""},
		{"unsupported device (real)", file("unsupported.json"), false, false, 0, "Unable to detect device type"},
		{"not json", []byte("Segmentation fault"), false, false, 0, "not JSON"},
		{"nvme media errors", nvme("true", "0", "7"), true, false, 7, ""},
		{"nvme gone read-only", nvme("false", "8", "0"), true, true, 0, "read-only"},
		{"nvme spare and temperature", nvme("true", "3", "0"), true, true, 0, "spare blocks below threshold, temperature out of range"},
		{"ata clean", ata("true", attr("5", "0")+","+attr("197", "0")+","+attr("9", "31000")), true, false, 0, ""},
		{"ata counted, the rest ignored", ata("true", attr("5", "8")+","+attr("187", "1")+","+attr("197", "2")+","+attr("198", "2")+","+attr("188", "4295032833")+","+attr("199", "40")), true, false, 13, ""},
		{"ata packed raw value", ata("true", attr("5", "281474976710660")), true, false, 4, ""},
		{"ata failed", ata("false", ""), true, true, 0, "FAILED"},
	} {
		s := parseSMART("/dev/x", c.raw)
		if s.Read != c.read || s.Failing != c.failing || s.Errors != c.errors || !strings.Contains(s.Why, c.why) {
			t.Errorf("%s: got %+v; want read=%v failing=%v errors=%d why~%q", c.name, s, c.read, c.failing, c.errors, c.why)
		}
	}
}

func TestSMARTCheck(t *testing.T) {
	for _, c := range []struct {
		s    SMART
		want Status
	}{
		{SMART{Why: "asleep"}, Warn}, // could not tell is never a pass
		{SMART{Read: true}, Pass},
		{SMART{Read: true, Errors: 2}, Warn},
		{SMART{Read: true, Failing: true, Errors: 2}, Refuse},
	} {
		if got := SMARTCheck(c.s); got.Status != c.want || got.Name != "disk-health" {
			t.Errorf("%+v: %+v, want %s", c.s, got, c.want)
		}
	}
}

func TestAssessLiveJudgesEachDisk(t *testing.T) {
	cs := AssessLive(LiveFacts{DiskFreeMB: 10 << 10, NTPSynced: "yes", Disks: []SMART{{Device: "/dev/a", Read: true}, {Device: "/dev/b", Read: true, Failing: true}}})
	var got []Status
	for _, c := range cs {
		if c.Name == "disk-health" {
			got = append(got, c.Status)
		}
	}
	if len(got) != 2 || got[0] != Pass || got[1] != Refuse {
		t.Fatalf("disk-health checks %v, want [pass refuse]", got)
	}
}
