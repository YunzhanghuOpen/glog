// Go support for leveled logs, analogous to https://code.google.com/p/google-glog/
//
// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package glog

import (
	"bytes"
	"fmt"
	stdLog "log"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRotateByTime(t *testing.T) {
	assert := assert.New(t)
	timeArray := [][5]string{
		{"2006-01-02 03:04:05 PM", "2006-02-01 12:00:00 AM", "2006-01-03 12:00:00 AM", "2006-01-02 04:00:00 PM", "2006-01-02 03:05:00 PM"},
		{"2019-12-31 11:59:59 PM", "2020-01-01 12:00:00 AM", "2020-01-01 12:00:00 AM", "2020-01-01 12:00:00 AM", "2020-01-01 12:00:00 AM"},
		{"2019-12-31 05:50:49 PM", "2020-01-01 12:00:00 AM", "2020-01-01 12:00:00 AM", "2019-12-31 06:00:00 PM", "2019-12-31 05:51:00 PM"},
		{"2019-01-31 08:14:58 PM", "2019-02-01 12:00:00 AM", "2019-02-01 12:00:00 AM", "2019-01-31 09:00:00 PM", "2019-01-31 08:15:00 PM"},
		{"2019-02-01 03:04:40 PM", "2019-03-01 12:00:00 AM", "2019-02-02 12:00:00 AM", "2019-02-01 04:00:00 PM", "2019-02-01 03:05:00 PM"},
		{"2019-02-28 03:04:31 PM", "2019-03-01 12:00:00 AM", "2019-03-01 12:00:00 AM", "2019-02-28 04:00:00 PM", "2019-02-28 03:05:00 PM"},
		{"2019-03-31 10:35:42 PM", "2019-04-01 12:00:00 AM", "2019-04-01 12:00:00 AM", "2019-03-31 11:00:00 PM", "2019-03-31 10:36:00 PM"},
		{"2019-04-01 01:00:00 AM", "2019-05-01 12:00:00 AM", "2019-04-02 12:00:00 AM", "2019-04-01 02:00:00 AM", "2019-04-01 01:01:00 AM"},
		{"2019-01-01 12:00:00 AM", "2019-02-01 12:00:00 AM", "2019-01-02 12:00:00 AM", "2019-01-01 01:00:00 AM", "2019-01-01 12:01:00 AM"},
	}

	for _, tlist := range timeArray {
		t, _ := time.Parse("2006-01-02 03:04:05 PM", tlist[0])
		*LogRotateInterval = "month"
		assert.Equal(tlist[1], getStartOfNextTime(t).Format("2006-01-02 03:04:05 PM"))
		*LogRotateInterval = "day"
		assert.Equal(tlist[2], getStartOfNextTime(t).Format("2006-01-02 03:04:05 PM"))
		*LogRotateInterval = "hour"
		assert.Equal(tlist[3], getStartOfNextTime(t).Format("2006-01-02 03:04:05 PM"))
		*LogRotateInterval = "minute"
		assert.Equal(tlist[4], getStartOfNextTime(t).Format("2006-01-02 03:04:05 PM"))
	}
}

// Test that shortHostname works as advertised.
func TestShortHostname(t *testing.T) {
	for hostname, expect := range map[string]string{
		"":                "",
		"host":            "host",
		"host.google.com": "host",
	} {
		if got := shortHostname(hostname); expect != got {
			t.Errorf("shortHostname(%q): expected %q, got %q", hostname, expect, got)
		}
	}
}

// flushBuffer wraps a bytes.Buffer to satisfy flushSyncWriter.
type flushBuffer struct {
	bytes.Buffer
}

func (f *flushBuffer) Flush() error {
	return nil
}

func (f *flushBuffer) Sync() error {
	return nil
}

// swap sets the log writers and returns the old array.
func (l *loggingT) swap(writers [numSeverity]flushSyncWriter) (old [numSeverity]flushSyncWriter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old = l.file
	for i, w := range writers {
		logging.file[i] = w
	}
	return
}

// newBuffers sets the log writers to all new byte buffers and returns the old array.
func (l *loggingT) newBuffers() [numSeverity]flushSyncWriter {
	return l.swap([numSeverity]flushSyncWriter{new(flushBuffer), new(flushBuffer), new(flushBuffer), new(flushBuffer)})
}

// contents returns the specified log value as a string.
func contents(s severity) string {
	return logging.file[s].(*flushBuffer).String()
}

// contains reports whether the string is contained in the log.
func contains(s severity, str string, t *testing.T) bool {
	return strings.Contains(contents(s), str)
}

// setFlags configures the logging flags how the test expects them.
func setFlags() {
	logging.toStderr = false
}

type Request struct {
	RawQuery   string
	RequestURI string
	Form       string
}

type TestStruct struct {
	Name         string
	CH_CARD_NO   string `db:"CH_CARD_NO" filter:"card"`
	CH_ID_CARD   string `db:"CH_ID_CARD" filter:"identity"`
	TestSliceMap map[string][]string
	TestMap      map[string]string
	TestReq      *Request
}

func TestFilter(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())

}

// Test that Info works as advertised.
func TestInfo(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())

	mRawQuery := "card_no=13231223123152346&id_card=98173648123548317823&mobile=13243562635"
	mRequestURI := "card_no=817624674236173234&id_card=62374818734678911&mobile=15951232523"
	mForm := "card_no=389561275823423468&id_card=2845283472472742346&mobile=18743724534"
	req := Request{
		RawQuery:   mRawQuery,
		RequestURI: mRequestURI,
		Form:       mForm,
	}
	mSliceMap := map[string][]string{
		"bank_code": {"18923755823466524", "6578259173782347234"},
		"bank_roae": {"468173871782375483", "47628128947889205"},
	}
	mMap := map[string]string{
		"card_no":   "19827676528372529846834",
		"alipay_id": "1773868729@qq.com",
		"cards_ss":  "18746572762342345623443",
	}
	cardNo, ID := "123897326471231263", "9399992392939293929392"
	mTestStruct := TestStruct{
		Name:         "Aline",
		CH_CARD_NO:   cardNo,
		CH_ID_CARD:   ID,
		TestSliceMap: mSliceMap,
		TestMap:      mMap,
		TestReq:      &req,
	}

	Info(mTestStruct)

	if !contains(infoLog, "I", t) {
		t.Errorf("Info has wrong character: %q", contents(infoLog))
	}
	if !contains(infoLog, ShrineCardNo(cardNo), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineIdentity(ID), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineIdentity("98173648123548317823"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineIdentity("62374818734678911"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "2845283472472742346", t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineCardNo("13231223123152346"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineCardNo("817624674236173234"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "389561275823423468", t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrinePhoneNumber("13243562635"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrinePhoneNumber("15951232523"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "389561275823423468", t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineCardNo("18923755823466524"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineCardNo("6578259173782347234"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "468173871782375483", t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "47628128947889205", t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineCardNo("19827676528372529846834"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, ShrineAlipayAccountNumber("1773868729@qq.com"), t) {
		t.Error("Info failed")
	}
	if !contains(infoLog, "18746572762342345623443", t) {
		t.Error("Info failed")
	}
}

func TestInfoDepth(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())

	f := func() { InfoDepth(1, "depth-test1") }

	// The next three lines must stay together
	_, _, wantLine, _ := runtime.Caller(0)
	InfoDepth(0, "depth-test0")
	f()

	msgs := strings.Split(strings.TrimSuffix(contents(infoLog), "\n"), "\n")
	if len(msgs) != 2 {
		t.Fatalf("Got %d lines, expected 2", len(msgs))
	}

	for i, m := range msgs {
		if !strings.HasPrefix(m, "I") {
			t.Errorf("InfoDepth[%d] has wrong character: %q", i, m)
		}
		w := fmt.Sprintf("depth-test%d", i)
		if !strings.Contains(m, w) {
			t.Errorf("InfoDepth[%d] missing %q: %q", i, w, m)
		}

		// pull out the line number (between : and ])
		msg := m[strings.LastIndex(m, ":")+1:]
		x := strings.Index(msg, "]")
		if x < 0 {
			t.Errorf("InfoDepth[%d]: missing ']': %q", i, m)
			continue
		}
		line, err := strconv.Atoi(msg[:x])
		if err != nil {
			t.Errorf("InfoDepth[%d]: bad line number: %q", i, m)
			continue
		}
		wantLine++
		if wantLine != line {
			t.Errorf("InfoDepth[%d]: got line %d, want %d", i, line, wantLine)
		}
	}
}

func init() {
	CopyStandardLogTo("INFO")
}

// Test that CopyStandardLogTo panics on bad input.
func TestCopyStandardLogToPanic(t *testing.T) {
	defer func() {
		if s, ok := recover().(string); !ok || !strings.Contains(s, "LOG") {
			t.Errorf(`CopyStandardLogTo("LOG") should have panicked: %v`, s)
		}
	}()
	CopyStandardLogTo("LOG")
}

// Test that using the standard log package logs to INFO.
func TestStandardLog(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	stdLog.Print("test")
	if !contains(infoLog, "I", t) {
		t.Errorf("Info has wrong character: %q", contents(infoLog))
	}
	if !contains(infoLog, "test", t) {
		t.Error("Info failed")
	}
}

// Test that the header has the correct format.
func TestHeader(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	defer func(previous func() time.Time) { timeNow = previous }(timeNow)
	timeNow = func() time.Time {
		return time.Date(2006, 1, 2, 15, 4, 5, .067890e9, time.Local)
	}
	pid = 1234
	Info("test")
	var line int
	format := "I0102 15:04:05.067890    1234 glog_test.go:%d] test\n"
	n, err := fmt.Sscanf(contents(infoLog), format, &line)
	if n != 1 || err != nil {
		t.Errorf("log format error: %d elements, error %s:\n%s", n, err, contents(infoLog))
	}
	// Scanf treats multiple spaces as equivalent to a single space,
	// so check for correct space-padding also.
	want := fmt.Sprintf(format, line)
	if contents(infoLog) != want {
		t.Errorf("log format error: got:\n\t%q\nwant:\t%q", contents(infoLog), want)
	}
}

// Test that an Error log goes to Warning and Info.
// Even in the Info log, the source character will be E, so the data should
// all be identical.
func TestError(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	Error("test")
	if !contains(errorLog, "E", t) {
		t.Errorf("Error has wrong character: %q", contents(errorLog))
	}
	if !contains(errorLog, "test", t) {
		t.Error("Error failed")
	}
	str := contents(errorLog)
	if !contains(warningLog, str, t) {
		t.Error("Warning failed")
	}
	if !contains(infoLog, str, t) {
		t.Error("Info failed")
	}
}

// Test that a Warning log goes to Info.
// Even in the Info log, the source character will be W, so the data should
// all be identical.
func TestWarning(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	Warning("test")
	if !contains(warningLog, "W", t) {
		t.Errorf("Warning has wrong character: %q", contents(warningLog))
	}
	if !contains(warningLog, "test", t) {
		t.Error("Warning failed")
	}
	str := contents(warningLog)
	if !contains(infoLog, str, t) {
		t.Error("Info failed")
	}
}

// Test that a V log goes to Info.
func TestV(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	logging.verbosity.Set("2")
	defer logging.verbosity.Set("0")
	V(2).Info("test")
	if !contains(infoLog, "I", t) {
		t.Errorf("Info has wrong character: %q", contents(infoLog))
	}
	if !contains(infoLog, "test", t) {
		t.Error("Info failed")
	}
}

// Test that a vmodule enables a log in this file.
func TestVmoduleOn(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	logging.vmodule.Set("glog_test=2")
	defer logging.vmodule.Set("")
	if !V(1) {
		t.Error("V not enabled for 1")
	}
	if !V(2) {
		t.Error("V not enabled for 2")
	}
	if V(3) {
		t.Error("V enabled for 3")
	}
	V(2).Info("test")
	if !contains(infoLog, "I", t) {
		t.Errorf("Info has wrong character: %q", contents(infoLog))
	}
	if !contains(infoLog, "test", t) {
		t.Error("Info failed")
	}
}

// Test that a vmodule of another file does not enable a log in this file.
func TestVmoduleOff(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	logging.vmodule.Set("notthisfile=2")
	defer logging.vmodule.Set("")
	for i := 1; i <= 3; i++ {
		if V(Level(i)) {
			t.Errorf("V enabled for %d", i)
		}
	}
	V(2).Info("test")
	if contents(infoLog) != "" {
		t.Error("V logged incorrectly")
	}
}

// vGlobs are patterns that match/don't match this file at V=2.
var vGlobs = map[string]bool{
	// Easy to test the numeric match here.
	"glog_test=1": false, // If -vmodule sets V to 1, V(2) will fail.
	"glog_test=2": true,
	"glog_test=3": true, // If -vmodule sets V to 1, V(3) will succeed.
	// These all use 2 and check the patterns. All are true.
	"*=2":           true,
	"?l*=2":         true,
	"????_*=2":      true,
	"??[mno]?_*t=2": true,
	// These all use 2 and check the patterns. All are false.
	"*x=2":         false,
	"m*=2":         false,
	"??_*=2":       false,
	"?[abc]?_*t=2": false,
}

// Test that vmodule globbing works as advertised.
func testVmoduleGlob(pat string, match bool, t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	defer logging.vmodule.Set("")
	logging.vmodule.Set(pat)
	if V(2) != Verbose(match) {
		t.Errorf("incorrect match for %q: got %t expected %t", pat, V(2), match)
	}
}

// Test that a vmodule globbing works as advertised.
func TestVmoduleGlob(t *testing.T) {
	for glob, match := range vGlobs {
		testVmoduleGlob(glob, match, t)
	}
}

func TestRollover(t *testing.T) {
	setFlags()
	var err error
	defer func(previous func(error)) { logExitFunc = previous }(logExitFunc)
	logExitFunc = func(e error) {
		err = e
	}
	defer func(previous uint64) { MaxSize = previous }(MaxSize)
	MaxSize = 512

	Info("x") // Be sure we have a file.
	info, ok := logging.file[infoLog].(*syncBuffer)
	if !ok {
		t.Fatal("info wasn't created")
	}
	if err != nil {
		t.Fatalf("info has initial error: %v", err)
	}
	fname0 := info.file.Name()
	Info(strings.Repeat("x", int(MaxSize))) // force a rollover
	if err != nil {
		t.Fatalf("info has error after big write: %v", err)
	}

	// Make sure the next log file gets a file name with a different
	// time stamp.
	//
	// TODO: determine whether we need to support subsecond log
	// rotation.  C++ does not appear to handle this case (nor does it
	// handle Daylight Savings Time properly).
	time.Sleep(1 * time.Second)

	Info("x") // create a new file
	if err != nil {
		t.Fatalf("error after rotation: %v", err)
	}
	fname1 := info.file.Name()
	if fname0 == fname1 {
		t.Errorf("info.f.Name did not change: %v", fname0)
	}
	if info.nbytes >= MaxSize {
		t.Errorf("file size was not reset: %d", info.nbytes)
	}
}

func TestLogBacktraceAt(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	// The peculiar style of this code simplifies line counting and maintenance of the
	// tracing block below.
	var infoLine string
	setTraceLocation := func(file string, line int, ok bool, delta int) {
		if !ok {
			t.Fatal("could not get file:line")
		}
		_, file = filepath.Split(file)
		infoLine = fmt.Sprintf("%s:%d", file, line+delta)
		err := logging.traceLocation.Set(infoLine)
		if err != nil {
			t.Fatal("error setting log_backtrace_at: ", err)
		}
	}
	{
		// Start of tracing block. These lines know about each other's relative position.
		_, file, line, ok := runtime.Caller(0)
		setTraceLocation(file, line, ok, +2) // Two lines between Caller and Info calls.
		Info("we want a stack trace here")
	}
	numAppearances := strings.Count(contents(infoLog), infoLine)
	if numAppearances < 2 {
		// Need 2 appearances, one in the log header and one in the trace:
		//   log_test.go:281: I0511 16:36:06.952398 02238 log_test.go:280] we want a stack trace here
		//   ...
		//   github.com/glog/glog_test.go:280 (0x41ba91)
		//   ...
		// We could be more precise but that would require knowing the details
		// of the traceback format, which may not be dependable.
		t.Fatal("got no trace back; log is ", contents(infoLog))
	}
}

func BenchmarkHeader(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buf, _, _ := logging.header(infoLog, 0)
		logging.putBuffer(buf)
	}
}

func TestTruncate(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())
	logging.maxLogMessageLen = 90
	Info("testmaxlogmessagelen1234567890测试中文哈哈哈哈哈哈哈哈哈哈哈")
	a := assert.New(t)
	message := contents(infoLog)
	a.Equal(len([]rune(message)), 90)
	a.True(strings.HasSuffix(message, "..."))
	a.Contains(message, "testmaxlogmessagelen1234567890测试中文哈哈哈哈哈...")
	logging.maxLogMessageLen = -1
	Info("testmaxlogmessagelen1234567890测试中文哈哈哈哈哈哈哈哈哈哈哈")
	message = contents(infoLog)
	a.Contains(message, "testmaxlogmessagelen1234567890测试中文哈哈哈哈哈哈哈哈哈哈哈")
	a.False(strings.HasSuffix(message, "..."))
	logging.maxLogMessageLen = 64
	Info("testmaxlogmessagelen1234567890测试中文哈哈哈哈哈哈哈哈哈哈哈")
	message = contents(infoLog)
	a.Contains(message, "testmaxlogmessagelen1234567890测试中文哈哈哈哈哈哈哈哈哈哈哈")
	a.False(strings.HasSuffix(message, "..."))
}

type T struct {
	SliceIfWithRealNameTag []interface{} `filter:"realname"`
	SliceIfWithoutRealNameTag []interface{}

	SliceStrWithRealNameTag []string `filter:"realname"`
	SliceStrWithoutRealNameTag []string
}

func TestSSliceEncrypt1(t *testing.T)  {
	setFlags()
	defer logging.swap(logging.newBuffers())

	//[]interface{} 元素是String
	val1 := T{
		SliceIfWithRealNameTag: []interface{}{
			"TI（加密）有限公司",
		},
		SliceIfWithoutRealNameTag: []interface{}{
			"TI（未加密）有限公司",
		},
		SliceStrWithRealNameTag: []string{
			"TS（加密）有限公司",
		},
		SliceStrWithoutRealNameTag: []string{
			"TS（未加密）有限公司",
		},
	}
	Info(val1)

	a := assert.New(t)
	message := contents(infoLog)

	a.Contains(message, ShrineRealName("TI（加密）有限公司"))
	a.Contains(message, "TI（未加密）有限公司")
	a.Contains(message, ShrineRealName("TS（加密）有限公司"))
	a.Contains(message, "TS（未加密）有限公司")
}
func TestSSliceEncrypt2(t *testing.T) {
	setFlags()
	defer logging.swap(logging.newBuffers())

	//[]interface{} 元素非String
	val2 := T{
		SliceIfWithRealNameTag: []interface{}{
			map[string]string{
				"1": "TIM（未加密）有限公司",
			},
		},
		SliceIfWithoutRealNameTag: []interface{}{
			[]string{
				"2", "TISlice（未加密）有限公司",
			},
			"TIString（未加密）有限公司",
		},
		SliceStrWithRealNameTag: []string{
			"TS（加密）有限公司",
		},
		SliceStrWithoutRealNameTag: []string{
			"TS（未加密）有限公司",
		},
	}
	Info(val2)

	a := assert.New(t)
	message := contents(infoLog)

	a.Contains(message, "TIM（未加密）有限公司")
	a.Contains(message, "TISlice（未加密）有限公司")
	a.Contains(message, "TIString（未加密）有限公司")
	a.Contains(message, ShrineRealName("TS（加密）有限公司"))
	a.Contains(message, "TS（未加密）有限公司")
}
func TestSSliceEncrypt3(t *testing.T)  {
	setFlags()
	defer logging.swap(logging.newBuffers())

	//嵌套结构体
	type T1 struct {
		T
		SliceStruWithRealNameTag []T `filter:"realname"`
		MapStruWithRealNameTag map[string]T `filter:"realname"`
	}
	val3 := T1{
		T: T{
			SliceIfWithRealNameTag: []interface{}{
				"TI（加密）有限公司",
			},
			SliceIfWithoutRealNameTag: []interface{}{
				"TI（未加密）有限公司",
			},
			SliceStrWithRealNameTag: []string{
				"TS（加密）有限公司",
			},
			SliceStrWithoutRealNameTag: []string{
				"TS（未加密）有限公司",
			},
		},
		SliceStruWithRealNameTag: []T{
			T{
				SliceIfWithRealNameTag: []interface{}{
					"STI Normal（加密）有限公司",
				},
				SliceIfWithoutRealNameTag: []interface{}{
					"STI Normal（未加密）有限公司",
				},
				SliceStrWithRealNameTag: []string{
					"STS Normal（加密）有限公司",
				},
				SliceStrWithoutRealNameTag: []string{
					"STS Normal（未加密）有限公司",
				},
			},
			T{
				SliceIfWithRealNameTag: []interface{}{
					1,
					"STI MIX（未加密1）有限公司",
				},
				SliceIfWithoutRealNameTag: []interface{}{
					2,
					"STI MIX（未加密2）有限公司",
				},
				SliceStrWithRealNameTag: []string{
					"STS MIX（加密）有限公司",
				},
				SliceStrWithoutRealNameTag: []string{
					"STS MIX（未加密）有限公司",
				},
			},
		},
		MapStruWithRealNameTag: map[string]T{
			"val4_1": 		T{
				SliceIfWithRealNameTag: []interface{}{
					"MTI Normal（加密）有限公司",
				},
				SliceIfWithoutRealNameTag: []interface{}{
					"MTI Normal（未加密）有限公司",
				},
				SliceStrWithRealNameTag: []string{
					"MTS Normal（加密）有限公司",
				},
				SliceStrWithoutRealNameTag: []string{
					"MTS Normal（未加密）有限公司",
				},
			},
			"val4_2": 		T{
				SliceIfWithRealNameTag: []interface{}{
					1,
					"MTI MIX（未加密1）有限公司",
				},
				SliceIfWithoutRealNameTag: []interface{}{
					2,
					"MTI MIX（未加密2）有限公司",
				},
			},
		},
	}
	Info(val3)

	a := assert.New(t)
	message := contents(infoLog)

	a.Contains(message, ShrineRealName("TI（加密）有限公司"))
	a.Contains(message, "TI（未加密）有限公司")
	a.Contains(message, ShrineRealName("TS（加密）有限公司"))
	a.Contains(message, "TS（未加密）有限公司")

	a.Contains(message, ShrineRealName("STI Normal（加密）有限公司"))
	a.Contains(message, "STI Normal（未加密）有限公司")
	a.Contains(message, ShrineRealName("STS Normal（加密）有限公司"))
	a.Contains(message, "STS Normal（未加密）有限公司")

	a.Contains(message, "STI MIX（未加密1）有限公司")
	a.Contains(message, "STI MIX（未加密2）有限公司")
	a.Contains(message, ShrineRealName("STS MIX（加密）有限公司"))
	a.Contains(message, "STS MIX（未加密）有限公司")

	a.Contains(message, ShrineRealName("MTI Normal（加密）有限公司"))
	a.Contains(message, "MTI Normal（未加密）有限公司")
	a.Contains(message, ShrineRealName("MTS Normal（加密）有限公司"))
	a.Contains(message, "MTS Normal（未加密）有限公司")
	a.Contains(message, "MTI MIX（未加密1）有限公司")
	a.Contains(message, "MTI MIX（未加密2）有限公司")
}