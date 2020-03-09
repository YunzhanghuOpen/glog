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

// Package glog implements logging analogous to the Google-internal C++ INFO/ERROR/V setup.
// It provides functions Info, Warning, Error, Fatal, plus formatting variants such as
// Infof. It also provides V-style logging controlled by the -v and -vmodule=file=2 flags.
//
// Basic examples:
//
//	glog.Info("Prepare to repel boarders")
//
//	glog.Fatalf("Initialization failed: %s", err)
//
// See the documentation for the V function for an explanation of these examples:
//
//	if glog.V(2) {
//		glog.Info("Starting transaction...")
//	}
//
//	glog.V(2).Infoln("Processed", nItems, "elements")
//
// Log output is buffered and written periodically using Flush. Programs
// should call Flush before exiting to guarantee all log output is written.
//
// By default, all log statements write to files in a temporary directory.
// This package provides several flags that modify this behavior.
// As a result, flag.Parse must be called before any logging is done.
//
//	-logtostderr=false
//		Logs are written to standard error instead of to files.
//	-alsologtostderr=false
//		Logs are written to standard error as well as to files.
//	-stderrthreshold=ERROR
//		Log events at or above this severity are logged to standard
//		error as well as to files.
//	-log_dir=""
//		Log files will be written to this directory instead of the
//		default temporary directory.
//
//	Other flags provide aids to debugging.
//
//	-log_backtrace_at=""
//		When set to a file and line number holding a logging statement,
//		such as
//			-log_backtrace_at=gopherflakes.go:234
//		a stack trace will be written to the Info log whenever execution
//		hits that statement. (Unlike with -vmodule, the ".go" must be
//		present.)
//	-v=0
//		Enable V-leveled logging at the specified level.
//	-vmodule=""
//		The syntax of the argument is a comma-separated list of pattern=N,
//		where pattern is a literal file name (minus the ".go" suffix) or
//		"glob" pattern and N is a V level. For instance,
//			-vmodule=gopher*=3
//		sets the V level to 3 in all Go files whose names begin "gopher".
//
package glog

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	stdLog "log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// severity identifies the sort of log: info, warning etc. It also implements
// the flag.Value interface. The -stderrthreshold flag is of type severity and
// should be modified only through the flag.Value interface. The values match
// the corresponding constants in C++.
type severity int32  // sync/atomic int32
type printtype int32 // print type int32
type shrinetype int  // shrine type int
// These constants identify the log levels in order of increasing severity.
// A message written to a high-severity log file is also written to each
// lower-severity log file.
const (
	infoLog severity = iota
	warningLog
	errorLog
	fatalLog
	numSeverity = 4

	tprint printtype = iota
	tprintln
	tprintf

	shrineCardType shrinetype = iota
	shrinePhoneType
)

const defaultFlushInterval = 30 * time.Second

// header length. log message contains header. eg. "I0102 15:04:05.067890    1234 glog_test.go: "
const headerLength = 64

var (
	Is2088BeginRe          = regexp.MustCompile(`^(2088)+[0-9]*$`)
	IsPhoneNumberRe        = regexp.MustCompile(`^(1[3-9][0-9]\d{8})$`)
	IsEmailRe              = regexp.MustCompile(`^([a-z_A-Z.0-9-])+@([a-zA-Z0-9_-])+\.([a-zA-Z0-9_-])+`)
	IsPhoneNumberCountryRe = regexp.MustCompile(`^\d{1,}\-\d{1,}$`)
	IsNumber               = regexp.MustCompile(`^[0-9]*$`)
)

const severityChar = "IWEF"

var severityName = []string{
	infoLog:    "INFO",
	warningLog: "WARNING",
	errorLog:   "ERROR",
	fatalLog:   "FATAL",
}

// get returns the value of the severity.
func (s *severity) get() severity {
	return severity(atomic.LoadInt32((*int32)(s)))
}

// set sets the value of the severity.
func (s *severity) set(val severity) {
	atomic.StoreInt32((*int32)(s), int32(val))
}

// String is part of the flag.Value interface.
func (s *severity) String() string {
	return strconv.FormatInt(int64(*s), 10)
}

// Get is part of the flag.Value interface.
func (s *severity) Get() interface{} {
	return *s
}

// Set is part of the flag.Value interface.
func (s *severity) Set(value string) error {
	var threshold severity
	// Is it a known name?
	if v, ok := severityByName(value); ok {
		threshold = v
	} else {
		v, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		threshold = severity(v)
	}
	logging.stderrThreshold.set(threshold)
	return nil
}

func severityByName(s string) (severity, bool) {
	s = strings.ToUpper(s)
	for i, name := range severityName {
		if name == s {
			return severity(i), true
		}
	}
	return 0, false
}

// OutputStats tracks the number of output lines and bytes written.
type OutputStats struct {
	lines int64
	bytes int64
}

// Lines returns the number of lines written.
func (s *OutputStats) Lines() int64 {
	return atomic.LoadInt64(&s.lines)
}

// Bytes returns the number of bytes written.
func (s *OutputStats) Bytes() int64 {
	return atomic.LoadInt64(&s.bytes)
}

// Stats tracks the number of lines of output and number of bytes
// per severity level. Values must be read with atomic.LoadInt64.
var Stats struct {
	Info, Warning, Error OutputStats
}

var severityStats = [numSeverity]*OutputStats{
	infoLog:    &Stats.Info,
	warningLog: &Stats.Warning,
	errorLog:   &Stats.Error,
}

// Level is exported because it appears in the arguments to V and is
// the type of the v flag, which can be set programmatically.
// It's a distinct type because we want to discriminate it from logType.
// Variables of type level are only changed under logging.mu.
// The -v flag is read only with atomic ops, so the state of the logging
// module is consistent.

// Level is treated as a sync/atomic int32.

// Level specifies a level of verbosity for V logs. *Level implements
// flag.Value; the -v flag is of type Level and should be modified
// only through the flag.Value interface.
type Level int32

// get returns the value of the Level.
func (l *Level) get() Level {
	return Level(atomic.LoadInt32((*int32)(l)))
}

// set sets the value of the Level.
func (l *Level) set(val Level) {
	atomic.StoreInt32((*int32)(l), int32(val))
}

// String is part of the flag.Value interface.
func (l *Level) String() string {
	return strconv.FormatInt(int64(*l), 10)
}

// Get is part of the flag.Value interface.
func (l *Level) Get() interface{} {
	return *l
}

// Set is part of the flag.Value interface.
func (l *Level) Set(value string) error {
	v, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	logging.mu.Lock()
	defer logging.mu.Unlock()
	logging.setVState(Level(v), logging.vmodule.filter, false)
	return nil
}

// moduleSpec represents the setting of the -vmodule flag.
type moduleSpec struct {
	filter []modulePat
}

// modulePat contains a filter for the -vmodule flag.
// It holds a verbosity level and a file pattern to match.
type modulePat struct {
	pattern string
	literal bool // The pattern is a literal string
	level   Level
}

// match reports whether the file matches the pattern. It uses a string
// comparison if the pattern contains no metacharacters.
func (m *modulePat) match(file string) bool {
	if m.literal {
		return file == m.pattern
	}
	match, _ := filepath.Match(m.pattern, file)
	return match
}

func (m *moduleSpec) String() string {
	// Lock because the type is not atomic. TODO: clean this up.
	logging.mu.Lock()
	defer logging.mu.Unlock()
	var b bytes.Buffer
	for i, f := range m.filter {
		if i > 0 {
			b.WriteRune(',')
		}
		fmt.Fprintf(&b, "%s=%d", f.pattern, f.level)
	}
	return b.String()
}

// Get is part of the (Go 1.2)  flag.Getter interface. It always returns nil for this flag type since the
// struct is not exported.
func (m *moduleSpec) Get() interface{} {
	return nil
}

var errVmoduleSyntax = errors.New("syntax error: expect comma-separated list of filename=N")

// Syntax: -vmodule=recordio=2,file=1,gfs*=3
func (m *moduleSpec) Set(value string) error {
	var filter []modulePat
	for _, pat := range strings.Split(value, ",") {
		if len(pat) == 0 {
			// Empty strings such as from a trailing comma can be ignored.
			continue
		}
		patLev := strings.Split(pat, "=")
		if len(patLev) != 2 || len(patLev[0]) == 0 || len(patLev[1]) == 0 {
			return errVmoduleSyntax
		}
		pattern := patLev[0]
		v, err := strconv.Atoi(patLev[1])
		if err != nil {
			return errors.New("syntax error: expect comma-separated list of filename=N")
		}
		if v < 0 {
			return errors.New("negative value for vmodule level")
		}
		if v == 0 {
			continue // Ignore. It's harmless but no point in paying the overhead.
		}
		// TODO: check syntax of filter?
		filter = append(filter, modulePat{pattern, isLiteral(pattern), Level(v)})
	}
	logging.mu.Lock()
	defer logging.mu.Unlock()
	logging.setVState(logging.verbosity, filter, true)
	return nil
}

// isLiteral reports whether the pattern is a literal string, that is, has no metacharacters
// that require filepath.Match to be called to match the pattern.
func isLiteral(pattern string) bool {
	return !strings.ContainsAny(pattern, `\*?[]`)
}

// traceLocation represents the setting of the -log_backtrace_at flag.
type traceLocation struct {
	file string
	line int
}

// isSet reports whether the trace location has been specified.
// logging.mu is held.
func (t *traceLocation) isSet() bool {
	return t.line > 0
}

// match reports whether the specified file and line matches the trace location.
// The argument file name is the full path, not the basename specified in the flag.
// logging.mu is held.
func (t *traceLocation) match(file string, line int) bool {
	if t.line != line {
		return false
	}
	if i := strings.LastIndex(file, "/"); i >= 0 {
		file = file[i+1:]
	}
	return t.file == file
}

func (t *traceLocation) String() string {
	// Lock because the type is not atomic. TODO: clean this up.
	logging.mu.Lock()
	defer logging.mu.Unlock()
	return fmt.Sprintf("%s:%d", t.file, t.line)
}

// Get is part of the (Go 1.2) flag.Getter interface. It always returns nil for this flag type since the
// struct is not exported
func (t *traceLocation) Get() interface{} {
	return nil
}

var errTraceSyntax = errors.New("syntax error: expect file.go:234")

// Syntax: -log_backtrace_at=gopherflakes.go:234
// Note that unlike vmodule the file extension is included here.
func (t *traceLocation) Set(value string) error {
	if value == "" {
		// Unset.
		t.line = 0
		t.file = ""
	}
	fields := strings.Split(value, ":")
	if len(fields) != 2 {
		return errTraceSyntax
	}
	file, line := fields[0], fields[1]
	if !strings.Contains(file, ".") {
		return errTraceSyntax
	}
	v, err := strconv.Atoi(line)
	if err != nil {
		return errTraceSyntax
	}
	if v <= 0 {
		return errors.New("negative or zero value for level")
	}
	logging.mu.Lock()
	defer logging.mu.Unlock()
	t.line = v
	t.file = file
	return nil
}

// flushSyncWriter is the interface satisfied by logging destinations.
type flushSyncWriter interface {
	Flush() error
	Sync() error
	io.Writer
}

func init() {
	flag.BoolVar(&logging.toStderr, "logtostderr", false, "log to standard error instead of files")
	flag.BoolVar(&logging.alsoToStderr, "alsologtostderr", false, "log to standard error as well as files")
	flag.Var(&logging.verbosity, "v", "log level for V logs")
	flag.Var(&logging.stderrThreshold, "stderrthreshold", "logs at or above this threshold go to stderr")
	flag.Var(&logging.vmodule, "vmodule", "comma-separated list of pattern=N settings for file-filtered logging")
	flag.Var(&logging.traceLocation, "log_backtrace_at", "when logging hits line file:N, emit a stack trace")
	flag.DurationVar(&logging.flushInterval, "flush_interval", defaultFlushInterval, "how often flush file")
	flag.IntVar(&logging.maxLogMessageLen, "maxlogmessagelen", -1, "when logging a very long log message, this value greater than 64 it will be truncate")

	// Default stderrThreshold is ERROR.
	logging.stderrThreshold = errorLog

	// Default filter card/salary/identity
	logging.SetFilter(true, true, true, true, true, true, true)

	logging.setVState(0, nil, false)
	go logging.flushDaemon()
}

// Flush flushes all pending log I/O.
func Flush() {
	logging.lockAndFlushAll()
}

// loggingT collects all the global state of the logging setup.
type loggingT struct {
	// Boolean flags. Not handled atomically because the flag.Value interface
	// does not let us avoid the =true, and that shorthand is necessary for
	// compatibility. TODO: does this matter enough to fix? Seems unlikely.
	toStderr     bool // The -logtostderr flag.
	alsoToStderr bool // The -alsologtostderr flag.

	// Level flag. Handled atomically.
	stderrThreshold severity // The -stderrthreshold flag.

	// freeList is a list of byte buffers, maintained under freeListMu.
	freeList *buffer
	// freeListMu maintains the free list. It is separate from the main mutex
	// so buffers can be grabbed and printed to without holding the main lock,
	// for better parallelization.
	freeListMu sync.Mutex

	// mu protects the remaining elements of this structure and is
	// used to synchronize logging.
	mu sync.Mutex
	// file holds writer for each of the log types.
	file [numSeverity]flushSyncWriter
	// pcs is used in V to avoid an allocation when computing the caller's PC.
	pcs [1]uintptr
	// vmap is a cache of the V Level for each V() call site, identified by PC.
	// It is wiped whenever the vmodule flag changes state.
	vmap map[uintptr]Level
	// filterLength stores the length of the vmodule filter chain. If greater
	// than zero, it means vmodule is enabled. It may be read safely
	// using sync.LoadInt32, but is only modified under mu.
	filterLength int32
	// traceLocation is the state of the -log_backtrace_at flag.
	traceLocation traceLocation
	// These flags are modified only under lock, although verbosity may be fetched
	// safely using atomic.LoadInt32.
	vmodule   moduleSpec // The state of the -vmodule flag.
	verbosity Level      // V logging level, the value of the -v flag/
	// how often flush file
	flushInterval time.Duration
	// usage:
	// type User struct {
	//     Name     string
	//     IDCard   string		`"filter":"identity"`
	//     Salary   int
	//     BankCard string		`"filter:"card""`
	// }
	// var user = User{"Jone", "17263862997372453", 17777, "836274728264816746"}
	// glog.Infoln(user)
	//
	// Output:
	//    if filterCard == true
	// >>>> {Jone 17263862997372453 17777 836274********6746}
	//    if filterIdentity == true
	// >>>> {Jone 172638*******2453 17777 836274728264816746}
	filterCard     bool
	filterIdentity bool
	filterPhone    bool
	filterRealName bool
	filterEmail    bool
	filterPwd      bool
	filterCompany  bool
	// truncate long log message. default -1, less than headerLength(64) will not truncate
	maxLogMessageLen int
}

// buffer holds a byte Buffer for reuse. The zero value is ready for use.
type buffer struct {
	bytes.Buffer
	tmp  [64]byte // temporary byte array for creating headers.
	next *buffer
}

var logging loggingT

func (l *loggingT) SetFilter(card, identity, phone, realName, email, pwd, company bool) {
	l.filterCard = card
	l.filterIdentity = identity
	l.filterPhone = phone
	l.filterRealName = realName
	l.filterEmail = email
	l.filterPwd = pwd
	l.filterCompany = company
}

func (l *loggingT) SetCardFilter(card bool) {
	l.filterCard = card
}

func (l *loggingT) SetIdentityFilter(identity bool) {
	l.filterIdentity = identity
}

func (l *loggingT) SetPhoneFilter(phone bool) {
	l.filterPhone = phone
}

// setVState sets a consistent state for V logging.
// l.mu is held.
func (l *loggingT) setVState(verbosity Level, filter []modulePat, setFilter bool) {
	// Turn verbosity off so V will not fire while we are in transition.
	logging.verbosity.set(0)
	// Ditto for filter length.
	atomic.StoreInt32(&logging.filterLength, 0)

	// Set the new filters and wipe the pc->Level map if the filter has changed.
	if setFilter {
		logging.vmodule.filter = filter
		logging.vmap = make(map[uintptr]Level)
	}

	// Things are consistent now, so enable filtering and verbosity.
	// They are enabled in order opposite to that in V.
	atomic.StoreInt32(&logging.filterLength, int32(len(filter)))
	logging.verbosity.set(verbosity)
}

// getBuffer returns a new, ready-to-use buffer.
func (l *loggingT) getBuffer() *buffer {
	l.freeListMu.Lock()
	b := l.freeList
	if b != nil {
		l.freeList = b.next
	}
	l.freeListMu.Unlock()
	if b == nil {
		b = new(buffer)
	} else {
		b.next = nil
		b.Reset()
	}
	return b
}

// putBuffer returns a buffer to the free list.
func (l *loggingT) putBuffer(b *buffer) {
	if b.Len() >= 256 {
		// Let big buffers die a natural death.
		return
	}
	l.freeListMu.Lock()
	b.next = l.freeList
	l.freeList = b
	l.freeListMu.Unlock()
}

var timeNow = time.Now // Stubbed out for testing.

/*
header formats a log header as defined by the C++ implementation.
It returns a buffer containing the formatted header and the user's file and line number.
The depth specifies how many stack frames above lives the source line to be identified in the log message.

Log lines have this form:
	Lmmdd hh:mm:ss.uuuuuu threadid file:line] msg...
where the fields are defined as follows:
	L                A single character, representing the log level (eg 'I' for INFO)
	mm               The month (zero padded; ie May is '05')
	dd               The day (zero padded)
	hh:mm:ss.uuuuuu  Time in hours, minutes and fractional seconds
	threadid         The space-padded thread ID as returned by GetTID()
	file             The file name
	line             The line number
	msg              The user-supplied message
*/
func (l *loggingT) header(s severity, depth int) (*buffer, string, int) {
	_, file, line, ok := runtime.Caller(3 + depth)
	if !ok {
		file = "???"
		line = 1
	} else {
		slash := strings.LastIndex(file, "/")
		if slash >= 0 {
			file = file[slash+1:]
		}
	}
	return l.formatHeader(s, file, line), file, line
}

// formatHeader formats a log header using the provided file name and line number.
func (l *loggingT) formatHeader(s severity, file string, line int) *buffer {
	now := timeNow()
	if line < 0 {
		line = 0 // not a real line number, but acceptable to someDigits
	}
	if s > fatalLog {
		s = infoLog // for safety.
	}
	buf := l.getBuffer()

	// Avoid Fprintf, for speed. The format is so simple that we can do it quickly by hand.
	// It's worth about 3X. Fprintf is hard.
	_, month, day := now.Date()
	hour, minute, second := now.Clock()
	// Lmmdd hh:mm:ss.uuuuuu threadid file:line]
	buf.tmp[0] = severityChar[s]
	buf.twoDigits(1, int(month))
	buf.twoDigits(3, day)
	buf.tmp[5] = ' '
	buf.twoDigits(6, hour)
	buf.tmp[8] = ':'
	buf.twoDigits(9, minute)
	buf.tmp[11] = ':'
	buf.twoDigits(12, second)
	buf.tmp[14] = '.'
	buf.nDigits(6, 15, now.Nanosecond()/1000, '0')
	buf.tmp[21] = ' '
	buf.nDigits(7, 22, pid, ' ') // TODO: should be TID
	buf.tmp[29] = ' '
	buf.Write(buf.tmp[:30])
	buf.WriteString(file)
	buf.tmp[0] = ':'
	n := buf.someDigits(1, line)
	buf.tmp[n+1] = ']'
	buf.tmp[n+2] = ' '
	buf.Write(buf.tmp[:n+3])
	return buf
}

// Some custom tiny helper functions to print the log header efficiently.

const digits = "0123456789"

// twoDigits formats a zero-prefixed two-digit integer at buf.tmp[i].
func (buf *buffer) twoDigits(i, d int) {
	buf.tmp[i+1] = digits[d%10]
	d /= 10
	buf.tmp[i] = digits[d%10]
}

// nDigits formats an n-digit integer at buf.tmp[i],
// padding with pad on the left.
// It assumes d >= 0.
func (buf *buffer) nDigits(n, i, d int, pad byte) {
	j := n - 1
	for ; j >= 0 && d > 0; j-- {
		buf.tmp[i+j] = digits[d%10]
		d /= 10
	}
	for ; j >= 0; j-- {
		buf.tmp[i+j] = pad
	}
}

// someDigits formats a zero-prefixed variable-width integer at buf.tmp[i].
func (buf *buffer) someDigits(i, d int) int {
	// Print into the top, then copy down. We know there's space for at least
	// a 10-digit number.
	j := len(buf.tmp)
	for {
		j--
		buf.tmp[j] = digits[d%10]
		d /= 10
		if d == 0 {
			break
		}
	}
	return copy(buf.tmp[i:], buf.tmp[j:])
}

func (l *loggingT) println(s severity, args ...interface{}) {
	buf, file, line := l.header(s, 0)
	if l.filterCard || l.filterIdentity || l.filterPhone {
		l.filter(tprintln, buf, "", args...)
	} else {
		fmt.Fprintln(buf, args...)
	}
	l.output(s, buf, file, line, false)
}

func (l *loggingT) print(s severity, args ...interface{}) {
	l.printDepth(s, 1, args...)
}

func (l *loggingT) printDepth(s severity, depth int, args ...interface{}) {
	buf, file, line := l.header(s, depth)
	if l.filterCard || l.filterIdentity || l.filterPhone {
		l.filter(tprint, buf, "", args...)
	} else {
		fmt.Fprint(buf, args...)
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(s, buf, file, line, false)
}

func (l *loggingT) printf(s severity, format string, args ...interface{}) {
	buf, file, line := l.header(s, 0)
	if l.filterCard || l.filterIdentity || l.filterPhone {
		l.filter(tprintf, buf, format, args...)
	} else {
		fmt.Fprintf(buf, format, args...)
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(s, buf, file, line, false)
}

func (l *loggingT) printfDepth(s severity, depth int, format string, args ...interface{}) {
	buf, file, line := l.header(s, depth)
	if l.filterCard || l.filterIdentity || l.filterPhone {
		l.filter(tprintf, buf, format, args...)
	} else {
		fmt.Fprint(buf, args...)
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(s, buf, file, line, false)
}

// filter - if type is struct, it's tag is card or identity or phone, mask this.
// handle if type is []*Type, *Type, Struct in log struct.
func (l *loggingT) filter(t printtype, buf io.Writer, format string, args ...interface{}) {
	if len(args) > 0 {
		for i := range args {
			args[i] = l.transform(args[i])
		}
	}

	switch t {
	case tprint:
		fmt.Fprint(buf, args...)
	case tprintln:
		fmt.Fprintln(buf, args...)
	case tprintf:
		fmt.Fprintf(buf, format, args...)
	}
}

func (l *loggingT) switchTag(val *reflect.Value, field *reflect.StructField, container map[string]interface{}, index int) {
	if val.CanInterface() && val.IsValid() {
		tag := field.Tag.Get("filter")
		name := field.Name
		str, ok := val.Interface().(string)
		switch {
		case tag == "card":
			if ok && l.filterCard {
				container[field.Name] = ShrineAlipayAccountNumber(str)
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "identity" || name == "IDCard":
			if ok && l.filterIdentity {
				container[field.Name] = ShrineIdentity(str)
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "phone" || name == "PhoneNo":
			if ok && l.filterPhone {
				container[field.Name] = ShrinePhoneNumber(str)
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "realname" || name == "RealName":
			if ok && l.filterRealName {
				container[field.Name] = ShrineRealName(str)
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "email":
			if ok && l.filterEmail {
				container[field.Name] = ShrineEmail(str)
			} else {
				container[field.Name] = val.Interface()
			}
		case name == "RawQuery", name == "RequestURI", name == "Referrer":
			if ok {
				container[field.Name] = l.shrineRequestField(str, "&", "=")
			} else {
				container[field.Name] = val.Interface()
			}
		case name == "BankNameNumber":
			if ok && l.filterCard {
				container[field.Name] = ShrineCommaStr(str, shrineCardType)
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "pwd":
			if ok && l.filterPwd {
				container[field.Name] = ShrinePwdStr()
			} else {
				container[field.Name] = val.Interface()
			}
		case tag == "company":
			if ok && l.filterCompany {
				container[field.Name] = ShrineCompanyName(str)
			} else {
				container[field.Name] = val.Interface()
			}
		default:
			container[field.Name] = val.Interface()
		}
	}
}

func (l *loggingT) switchTagSlice(val *reflect.Value, field *reflect.StructField, container *[]interface{}, index int) {
	if val.CanInterface() && val.IsValid() {
		tag := field.Tag.Get("filter")
		name := field.Name
		str, _ := val.Interface().(string)
		switch {
		case tag == "card":
			*container = append(*container, ShrineAlipayAccountNumber(str))
		case tag == "identity" || name == "IDCard":
			*container = append(*container, ShrineIdentity(str))
		case tag == "phone" || name == "PhoneNo":
			*container = append(*container, ShrinePhoneNumber(str))
		case tag == "realname" || name == "RealName":
			*container = append(*container, ShrineRealName(str))
		case tag == "email":
			*container = append(*container, ShrineEmail(str))
		case name == "RawQuery", name == "RequestURI", name == "Referrer":
			*container = append(*container, val.Interface())
		case name == "BankNameNumber":
			*container = append(*container, ShrineCommaStr(str, shrineCardType))
		case tag == "pwd":
			*container = append(*container, ShrinePwdStr())
		case tag == "company":
			*container = append(*container, ShrineCompanyName(str))
		default:
			*container = append(*container, val.Interface())
		}
	}
}
// transform - Filtering the bank card number, the id card number
// and the mobile phone number through the reflection structure tag
// and slice key
func (l *loggingT) transform(v interface{}) interface{} {
	if _, ok := v.(error); ok {
		return v
	}
	// returns the value that v points to.
	val := reflect.Indirect(reflect.ValueOf(v))
	// Avoid panic
	if !val.IsValid() || !val.CanInterface() {
		return v
	}

	methodVal := val.MethodByName("Format")

	if methodVal.IsValid() {
		return v
	}

	switch val.Kind() {
	case reflect.Struct:
		// ret used to temporarily store information to be printed
		ret := make(map[string]interface{}, val.NumField())
		for i := 0; i < val.NumField(); i++ {
			outerVal := reflect.Indirect(val.Field(i))

			if !outerVal.IsValid() || !outerVal.CanInterface() {
				continue
			}
			field := val.Type().Field(i)

			if outerVal.Kind() == reflect.Interface {
				outerVal = reflect.Indirect(reflect.ValueOf(outerVal.Interface()))
			}

			if !outerVal.IsValid() || !outerVal.CanInterface() {
				continue
			}

			switch outerVal.Kind() {
			case reflect.Map, reflect.Array, reflect.Struct, reflect.Interface:
				ret[field.Name] = l.transform(outerVal.Interface())
			case reflect.Slice:
				strSliceFlag := false
				tmpSlice := make([]interface{}, 0)

				for idx := 0; idx < outerVal.Len(); idx++ {
					innerVal := reflect.Indirect(outerVal.Index(idx))
					if !innerVal.IsValid() || !innerVal.CanInterface() {
						continue
					}

					if innerVal.Kind() == reflect.Interface {
						innerVal = reflect.Indirect(reflect.ValueOf(innerVal.Interface()))
					}

					if !innerVal.IsValid() || !innerVal.CanInterface() {
						continue
					}

					switch innerVal.Kind() {
					case reflect.String:
						l.switchTagSlice(&innerVal, &field, &tmpSlice, idx)
					default:
						strSliceFlag = true
						break
					}
				}
				if strSliceFlag {
					ret[field.Name] = l.transform(outerVal.Interface())
				} else {
					ret[field.Name] = tmpSlice
				}
			case reflect.String:
				l.switchTag(&outerVal, &field, ret, i)
			default:
				ret[field.Name] = outerVal.Interface()
			}
		}
		return ret
	case reflect.Map:
		ret := make(map[string]interface{}, val.Len())
		keys := val.MapKeys()
		for _, key := range keys {
			if !key.IsValid() || !key.CanInterface() {
				continue
			}
			keyStr, ok := key.Interface().(string)

			if !ok {
				continue
			}
			mapVal := reflect.Indirect(val.MapIndex(key))

			if !mapVal.IsValid() || !mapVal.CanInterface() {
				continue
			}

			if mapVal.Kind() == reflect.Interface {
				mapVal = reflect.Indirect(reflect.ValueOf(mapVal.Interface()))
			}

			if !mapVal.IsValid() || !mapVal.CanInterface() {
				continue
			}

			switch mapVal.Kind() {
			case reflect.Map, reflect.Array, reflect.Struct, reflect.Interface:
				ret[keyStr] = l.transform(mapVal.Interface())
			case reflect.Slice:
				// for handle type of map[string][]string
				strSliceFlag := false
				tmpSlice := make([]interface{}, mapVal.Len())

				for i := 0; i < mapVal.Len(); i++ {
					innerVal := reflect.Indirect(mapVal.Index(i))
					if !innerVal.IsValid() || !innerVal.CanInterface() {
						continue
					}

					if innerVal.Kind() == reflect.Interface {
						innerVal = reflect.Indirect(reflect.ValueOf(innerVal.Interface()))
					}

					if !innerVal.IsValid() || !innerVal.CanInterface() {
						continue
					}

					switch innerVal.Kind() {
					case reflect.String:
						strSliceFlag = true

						// handle type of map[string][]string, map["bank_code"] = ["612846129387468123", "62194621124345826"]
						switch keyStr {
						case "bank_code", "bank_card", "alipay_id", "card_no", "cardNo":
							tmpSlice[i] = ShrineAlipayAccountNumber(innerVal.Interface().(string))
						case "id_card", "IDCard":
							tmpSlice[i] = ShrineIdentity(innerVal.Interface().(string))
						case "phone_no", "mobile":
							tmpSlice[i] = ShrinePhoneNumber(innerVal.Interface().(string))
						case "realname", "real_name", "realName":
							tmpSlice[i] = ShrineRealName(innerVal.Interface().(string))
						case "email":
							tmpSlice[i] = ShrineEmail(innerVal.Interface().(string))
						case "password", "pwd":
							tmpSlice[i] = ShrinePwdStr()
						case "dealer_name", "dealer_product_name", "broker_product_name", "product_name", "broker_name", "enterprise_name", "company":
							tmpSlice[i] = ShrineCompanyName(innerVal.Interface().(string))
						default:
							tmpSlice[i] = innerVal.Interface()
						}
					default:
						break
					}
				}

				if strSliceFlag {
					ret[keyStr] = tmpSlice
				} else {
					ret[keyStr] = l.transform(mapVal.Interface())
				}
			case reflect.String:
				// handle type of map[string]string, map["card_no"] = "612846129387468123" etc.
				haveCard := keyStr == "card_no" || keyStr == "bank_card" || keyStr == "cardNo" || strings.Contains(keyStr, "CardNo") ||
					strings.Contains(keyStr, "bank_code") || strings.Contains(keyStr, "acct_id") || strings.Contains(keyStr, "bank_branch") ||
					strings.Contains(keyStr, "ali_opponent_id") || strings.Contains(keyStr, "alipay_id") || strings.Contains(keyStr, "AlipayId")
				haveBankInfo := keyStr == "customer_bank_info" || keyStr == "producer_bank_info" || keyStr == "CH_PRODUCER_BANK_INFO" || keyStr == "CH_CUSTOMER_BANK_INFO"
				haveIDCard := keyStr == "id_card" || strings.Contains(keyStr, "IdCard") || strings.Contains(keyStr, "IDCard")
				havePhone := strings.Contains(keyStr, "phone") || strings.Contains(keyStr, "Phone") || strings.Contains(keyStr, "PHONE") ||
					strings.Contains(keyStr, "mobile") || strings.Contains(keyStr, "Mobile")
				haveAddrTel := keyStr == "producer_address_tel" || keyStr == "customer_address_tel" || keyStr == "CH_PRODUCER_ADDRESS_TEL" || keyStr == "CH_CUSTOMER_ADDRESS_TEL"
				haveRealName := keyStr == "realname" || keyStr == "real_name" || keyStr == "realName"
				haveEmail := keyStr == "email" || keyStr == "Email" || strings.Contains(keyStr, "EMAIL")
				havePwd := strings.Contains(strings.ToUpper(keyStr), "PWD") || strings.Contains(strings.ToUpper(keyStr), "PASSWORD")
				haveCompany := strings.Contains(strings.ToUpper(keyStr), "PRODUCT_NAME") || strings.Contains(strings.ToUpper(keyStr), "BROKER_NAME") || strings.Contains(strings.ToUpper(keyStr), "DEALER_NAME") || strings.Contains(strings.ToUpper(keyStr), "ENTERPRISE_NAME")

				switch {
				case haveCard:
					if l.filterCard {
						ret[keyStr] = ShrineAlipayAccountNumber(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveIDCard:
					if l.filterIdentity {
						ret[keyStr] = ShrineIdentity(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case havePhone:
					if l.filterPhone {
						ret[keyStr] = ShrinePhoneNumber(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveBankInfo:
					if l.filterCard {
						ret[keyStr] = ShrineCommaStr(mapVal.Interface().(string), shrineCardType)
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveAddrTel:
					if l.filterPhone {
						ret[keyStr] = ShrineCommaStr(mapVal.Interface().(string), shrinePhoneType)
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveRealName:
					if l.filterRealName {
						ret[keyStr] = ShrineRealName(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveEmail:
					if l.filterEmail {
						ret[keyStr] = ShrineEmail(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case havePwd:
					if l.filterPwd {
						ret[keyStr] = ShrinePwdStr()
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				case haveCompany:
					if l.filterCompany {
						ret[keyStr] = ShrineCompanyName(mapVal.Interface().(string))
					} else {
						ret[keyStr] = mapVal.Interface()
					}
				default:
					ret[keyStr] = mapVal.Interface()
				}
			default:
				ret[keyStr] = mapVal.Interface()
			}
		}
		return ret
	case reflect.Array, reflect.Slice:
		ret := make([]interface{}, val.Len())
		for i := 0; i < val.Len(); i++ {
			outerVal := reflect.Indirect(val.Index(i))
			if !outerVal.IsValid() || !outerVal.CanInterface() {
				continue
			}

			if outerVal.Kind() == reflect.Interface {
				outerVal = reflect.Indirect(reflect.ValueOf(outerVal.Interface()))
			}

			if !outerVal.IsValid() || !outerVal.CanInterface() {
				continue
			}

			switch outerVal.Kind() {
			case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Interface:
				ret[i] = l.transform(outerVal.Interface())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				return v
			default:
				ret[i] = outerVal.Interface()
			}
		}
		return ret
	case reflect.Interface:
		tempVal := val.Interface()
		return l.transform(tempVal)
	default:
		return v
	}

	return v
}

// printWithFileLine behaves like print but uses the provided file and line number.  If
// alsoLogToStderr is true, the log message always appears on standard error; it
// will also appear in the log file unless --logtostderr is set.
func (l *loggingT) printWithFileLine(s severity, file string, line int, alsoToStderr bool, args ...interface{}) {
	buf := l.formatHeader(s, file, line)
	fmt.Fprint(buf, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(s, buf, file, line, alsoToStderr)
}

// output writes the data to the log files and releases the buffer.
func (l *loggingT) output(s severity, buf *buffer, file string, line int, alsoToStderr bool) {
	l.mu.Lock()
	if l.traceLocation.isSet() {
		if l.traceLocation.match(file, line) {
			buf.Write(stacks(false))
		}
	}
	data := buf.Bytes()

	if l.maxLogMessageLen > headerLength {
		runes := []rune(string(data))
		if len(runes) > l.maxLogMessageLen {
			data = []byte(string(runes[:l.maxLogMessageLen-3]) + "...\n")
		}
	}
	if !flag.Parsed() {
		os.Stderr.Write([]byte("ERROR: logging before flag.Parse: "))
		os.Stderr.Write(data)
	} else if l.toStderr {
		os.Stderr.Write(data)
	} else {
		if alsoToStderr || l.alsoToStderr || s >= l.stderrThreshold.get() {
			os.Stderr.Write(data)
		}
		if l.file[s] == nil {
			if err := l.createFiles(s); err != nil {
				os.Stderr.Write(data) // Make sure the message appears somewhere.
				l.exit(err)
			}
		}
		switch s {
		case fatalLog:
			l.file[fatalLog].Write(data)
			fallthrough
		case errorLog:
			l.file[errorLog].Write(data)
			fallthrough
		case warningLog:
			l.file[warningLog].Write(data)
			fallthrough
		case infoLog:
			l.file[infoLog].Write(data)
		}
	}
	if s == fatalLog {
		// If we got here via Exit rather than Fatal, print no stacks.
		if atomic.LoadUint32(&fatalNoStacks) > 0 {
			l.mu.Unlock()
			timeoutFlush(10 * time.Second)
			os.Exit(1)
		}
		// Dump all goroutine stacks before exiting.
		// First, make sure we see the trace for the current goroutine on standard error.
		// If -logtostderr has been specified, the loop below will do that anyway
		// as the first stack in the full dump.
		if !l.toStderr {
			os.Stderr.Write(stacks(false))
		}
		// Write the stack trace for all goroutines to the files.
		trace := stacks(true)
		logExitFunc = func(error) {} // If we get a write error, we'll still exit below.
		for log := fatalLog; log >= infoLog; log-- {
			if f := l.file[log]; f != nil { // Can be nil if -logtostderr is set.
				f.Write(trace)
			}
		}
		l.mu.Unlock()
		timeoutFlush(10 * time.Second)
		os.Exit(255) // C++ uses -1, which is silly because it's anded with 255 anyway.
	}
	l.putBuffer(buf)
	l.mu.Unlock()
	if stats := severityStats[s]; stats != nil {
		atomic.AddInt64(&stats.lines, 1)
		atomic.AddInt64(&stats.bytes, int64(len(data)))
	}
}

// timeoutFlush calls Flush and returns when it completes or after timeout
// elapses, whichever happens first.  This is needed because the hooks invoked
// by Flush may deadlock when glog.Fatal is called from a hook that holds
// a lock.
func timeoutFlush(timeout time.Duration) {
	done := make(chan bool, 1)
	go func() {
		Flush() // calls logging.lockAndFlushAll()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Fprintln(os.Stderr, "glog: Flush took longer than", timeout)
	}
}

// stacks is a wrapper for runtime.Stack that attempts to recover the data for all goroutines.
func stacks(all bool) []byte {
	// We don't know how big the traces are, so grow a few times if they don't fit. Start large, though.
	n := 10000
	if all {
		n = 100000
	}
	var trace []byte
	for i := 0; i < 5; i++ {
		trace = make([]byte, n)
		nbytes := runtime.Stack(trace, all)
		if nbytes < len(trace) {
			return trace[:nbytes]
		}
		n *= 2
	}
	return trace
}

// logExitFunc provides a simple mechanism to override the default behavior
// of exiting on error. Used in testing and to guarantee we reach a required exit
// for fatal logs. Instead, exit could be a function rather than a method but that
// would make its use clumsier.
var logExitFunc func(error)

// exit is called if there is trouble creating or writing log files.
// It flushes the logs and exits the program; there's no point in hanging around.
// l.mu is held.
func (l *loggingT) exit(err error) {
	fmt.Fprintf(os.Stderr, "log: exiting because of error: %s\n", err)
	// If logExitFunc is set, we do that instead of exiting.
	if logExitFunc != nil {
		logExitFunc(err)
		return
	}
	l.flushAll()
	os.Exit(2)
}

// syncBuffer joins a bufio.Writer to its underlying file, providing access to the
// file's Sync method and providing a wrapper for the Write method that provides log
// file rotation. There are conflicting methods, so the file cannot be embedded.
// l.mu is held for all its methods.
type syncBuffer struct {
	logger *loggingT
	*bufio.Writer
	file           *os.File
	sev            severity
	nbytes         uint64    // The number of bytes written to this file
	nextRotateTime time.Time // Time of next rotate
}

func (sb *syncBuffer) Sync() error {
	return sb.file.Sync()
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	if sb.shouldRotateFile(uint64(len(p))) {
		if err := sb.rotateFile(time.Now()); err != nil {
			sb.logger.exit(err)
		}
	}
	n, err = sb.Writer.Write(p)
	sb.nbytes += uint64(n)
	if err != nil {
		sb.logger.exit(err)
	}
	return
}

// shouldRotateFile check whether should rotate file
func (sb *syncBuffer) shouldRotateFile(l uint64) bool {
	return sb.nbytes+l >= MaxSize ||
		!time.Now().Before(sb.nextRotateTime)
}

// rotateFile closes the syncBuffer's file and starts a new one.
func (sb *syncBuffer) rotateFile(now time.Time) error {
	if sb.file != nil {
		sb.Flush()
		sb.file.Close()
	}
	var err error
	sb.file, _, err = create(severityName[sb.sev], now)
	sb.nbytes = 0
	sb.nextRotateTime = getStartOfNextTime(time.Now())
	if err != nil {
		return err
	}

	sb.Writer = bufio.NewWriterSize(sb.file, bufferSize)

	// Write header.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Log file created at: %s\n", now.Format("2006/01/02 15:04:05"))
	fmt.Fprintf(&buf, "Running on machine: %s\n", host)
	fmt.Fprintf(&buf, "Binary: Built with %s %s for %s/%s\n", runtime.Compiler, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&buf, "Log line format: [IWEF]mmdd hh:mm:ss.uuuuuu threadid file:line] msg\n")
	n, err := sb.file.Write(buf.Bytes())
	sb.nbytes += uint64(n)
	return err
}

// bufferSize sizes the buffer associated with each log file. It's large
// so that log records can accumulate without the logging thread blocking
// on disk I/O. The flushDaemon will block instead.
const bufferSize = 256 * 1024

// createFiles creates all the log files for severity from sev down to infoLog.
// l.mu is held.
func (l *loggingT) createFiles(sev severity) error {
	now := time.Now()
	// Files are created in decreasing severity order, so as soon as we find one
	// has already been created, we can stop.
	for s := sev; s >= infoLog && l.file[s] == nil; s-- {
		sb := &syncBuffer{
			logger: l,
			sev:    s,
		}
		if err := sb.rotateFile(now); err != nil {
			return err
		}
		l.file[s] = sb
	}
	return nil
}

// flushDaemon periodically flushes the log file buffers.
func (l *loggingT) flushDaemon() {
	if l.flushInterval < time.Second {
		l.flushInterval = time.Second
	}
	for _ = range time.NewTicker(l.flushInterval).C {
		l.lockAndFlushAll()
	}
}

// lockAndFlushAll is like flushAll but locks l.mu first.
func (l *loggingT) lockAndFlushAll() {
	l.mu.Lock()
	l.flushAll()
	l.mu.Unlock()
}

// flushAll flushes all the logs and attempts to "sync" their data to disk.
// l.mu is held.
func (l *loggingT) flushAll() {
	// Flush from fatal down, in case there's trouble flushing.
	for s := fatalLog; s >= infoLog; s-- {
		file := l.file[s]
		if file != nil {
			file.Flush() // ignore error
			file.Sync()  // ignore error
		}
	}
}

// CopyStandardLogTo arranges for messages written to the Go "log" package's
// default logs to also appear in the Google logs for the named and lower
// severities.  Subsequent changes to the standard log's default output location
// or format may break this behavior.
//
// Valid names are "INFO", "WARNING", "ERROR", and "FATAL".  If the name is not
// recognized, CopyStandardLogTo panics.
func CopyStandardLogTo(name string) {
	sev, ok := severityByName(name)
	if !ok {
		panic(fmt.Sprintf("log.CopyStandardLogTo(%q): unrecognized severity name", name))
	}
	// Set a log format that captures the user's file and line:
	//   d.go:23: message
	stdLog.SetFlags(stdLog.Lshortfile)
	stdLog.SetOutput(logBridge(sev))
}

// logBridge provides the Write method that enables CopyStandardLogTo to connect
// Go's standard logs to the logs provided by this package.
type logBridge severity

// Write parses the standard logging line and passes its components to the
// logger for severity(lb).
func (lb logBridge) Write(b []byte) (n int, err error) {
	var (
		file = "???"
		line = 1
		text string
	)
	// Split "d.go:23: message" into "d.go", "23", and "message".
	if parts := bytes.SplitN(b, []byte{':'}, 3); len(parts) != 3 || len(parts[0]) < 1 || len(parts[2]) < 1 {
		text = fmt.Sprintf("bad log format: %s", b)
	} else {
		file = string(parts[0])
		text = string(parts[2][1:]) // skip leading space
		line, err = strconv.Atoi(string(parts[1]))
		if err != nil {
			text = fmt.Sprintf("bad line number: %s", b)
			line = 1
		}
	}
	// printWithFileLine with alsoToStderr=true, so standard log messages
	// always appear on standard error.
	logging.printWithFileLine(severity(lb), file, line, true, text)
	return len(b), nil
}

// setV computes and remembers the V level for a given PC
// when vmodule is enabled.
// File pattern matching takes the basename of the file, stripped
// of its .go suffix, and uses filepath.Match, which is a little more
// general than the *? matching used in C++.
// l.mu is held.
func (l *loggingT) setV(pc uintptr) Level {
	fn := runtime.FuncForPC(pc)
	file, _ := fn.FileLine(pc)
	// The file is something like /a/b/c/d.go. We want just the d.
	if strings.HasSuffix(file, ".go") {
		file = file[:len(file)-3]
	}
	if slash := strings.LastIndex(file, "/"); slash >= 0 {
		file = file[slash+1:]
	}
	for _, filter := range l.vmodule.filter {
		if filter.match(file) {
			l.vmap[pc] = filter.level
			return filter.level
		}
	}
	l.vmap[pc] = 0
	return 0
}

// Verbose is a boolean type that implements Infof (like Printf) etc.
// See the documentation of V for more information.
type Verbose bool

// V reports whether verbosity at the call site is at least the requested level.
// The returned value is a boolean of type Verbose, which implements Info, Infoln
// and Infof. These methods will write to the Info log if called.
// Thus, one may write either
//	if glog.V(2) { glog.Info("log this") }
// or
//	glog.V(2).Info("log this")
// The second form is shorter but the first is cheaper if logging is off because it does
// not evaluate its arguments.
//
// Whether an individual call to V generates a log record depends on the setting of
// the -v and --vmodule flags; both are off by default. If the level in the call to
// V is at least the value of -v, or of -vmodule for the source file containing the
// call, the V call will log.
func V(level Level) Verbose {
	// This function tries hard to be cheap unless there's work to do.
	// The fast path is two atomic loads and compares.

	// Here is a cheap but safe test to see if V logging is enabled globally.
	if logging.verbosity.get() >= level {
		return Verbose(true)
	}

	// It's off globally but it vmodule may still be set.
	// Here is another cheap but safe test to see if vmodule is enabled.
	if atomic.LoadInt32(&logging.filterLength) > 0 {
		// Now we need a proper lock to use the logging structure. The pcs field
		// is shared so we must lock before accessing it. This is fairly expensive,
		// but if V logging is enabled we're slow anyway.
		logging.mu.Lock()
		defer logging.mu.Unlock()
		if runtime.Callers(2, logging.pcs[:]) == 0 {
			return Verbose(false)
		}
		v, ok := logging.vmap[logging.pcs[0]]
		if !ok {
			v = logging.setV(logging.pcs[0])
		}
		return Verbose(v >= level)
	}
	return Verbose(false)
}

// Info is equivalent to the global Info function, guarded by the value of v.
// See the documentation of V for usage.
func (v Verbose) Info(args ...interface{}) {
	if v {
		logging.print(infoLog, args...)
	}
}

// Infoln is equivalent to the global Infoln function, guarded by the value of v.
// See the documentation of V for usage.
func (v Verbose) Infoln(args ...interface{}) {
	if v {
		logging.println(infoLog, args...)
	}
}

// Infof is equivalent to the global Infof function, guarded by the value of v.
// See the documentation of V for usage.
func (v Verbose) Infof(format string, args ...interface{}) {
	if v {
		logging.printf(infoLog, format, args...)
	}
}

// Info logs to the INFO log.
// Arguments are handled in the manner of fmt.Print; a newline is appended if missing.
func Info(args ...interface{}) {
	logging.print(infoLog, args...)
}

// InfoDepth acts as Info but uses depth to determine which call frame to log.
// InfoDepth(0, "msg") is the same as Info("msg").
func InfoDepth(depth int, args ...interface{}) {
	logging.printDepth(infoLog, depth, args...)
}

// Infoln logs to the INFO log.
// Arguments are handled in the manner of fmt.Println; a newline is appended if missing.
func Infoln(args ...interface{}) {
	logging.println(infoLog, args...)
}

// Infof logs to the INFO log.
// Arguments are handled in the manner of fmt.Printf; a newline is appended if missing.
func Infof(format string, args ...interface{}) {
	logging.printf(infoLog, format, args...)
}

// InfofDepth use format and depth print together
func InfofDepth(depth int, format string, args ...interface{}) {
	logging.printfDepth(infoLog, depth, format, args...)
}

// Warning logs to the WARNING and INFO logs.
// Arguments are handled in the manner of fmt.Print; a newline is appended if missing.
func Warning(args ...interface{}) {
	logging.print(warningLog, args...)
}

// WarningDepth acts as Warning but uses depth to determine which call frame to log.
// WarningDepth(0, "msg") is the same as Warning("msg").
func WarningDepth(depth int, args ...interface{}) {
	logging.printDepth(warningLog, depth, args...)
}

// Warningln logs to the WARNING and INFO logs.
// Arguments are handled in the manner of fmt.Println; a newline is appended if missing.
func Warningln(args ...interface{}) {
	logging.println(warningLog, args...)
}

// Warningf logs to the WARNING and INFO logs.
// Arguments are handled in the manner of fmt.Printf; a newline is appended if missing.
func Warningf(format string, args ...interface{}) {
	logging.printf(warningLog, format, args...)
}

// WarningfDepth use format and depth print together
func WarningfDepth(depth int, format string, args ...interface{}) {
	logging.printfDepth(warningLog, depth, format, args...)
}

// Error logs to the ERROR, WARNING, and INFO logs.
// Arguments are handled in the manner of fmt.Print; a newline is appended if missing.
func Error(args ...interface{}) {
	logging.print(errorLog, args...)
}

// ErrorDepth acts as Error but uses depth to determine which call frame to log.
// ErrorDepth(0, "msg") is the same as Error("msg").
func ErrorDepth(depth int, args ...interface{}) {
	logging.printDepth(errorLog, depth, args...)
}

// Errorln logs to the ERROR, WARNING, and INFO logs.
// Arguments are handled in the manner of fmt.Println; a newline is appended if missing.
func Errorln(args ...interface{}) {
	logging.println(errorLog, args...)
}

// Errorf logs to the ERROR, WARNING, and INFO logs.
// Arguments are handled in the manner of fmt.Printf; a newline is appended if missing.
func Errorf(format string, args ...interface{}) {
	logging.printf(errorLog, format, args...)
}

// ErrorfDepth use format and depth print together
func ErrorfDepth(depth int, format string, args ...interface{}) {
	logging.printfDepth(errorLog, depth, format, args...)
}

// Fatal logs to the FATAL, ERROR, WARNING, and INFO logs,
// including a stack trace of all running goroutines, then calls os.Exit(255).
// Arguments are handled in the manner of fmt.Print; a newline is appended if missing.
func Fatal(args ...interface{}) {
	logging.print(fatalLog, args...)
}

// FatalDepth acts as Fatal but uses depth to determine which call frame to log.
// FatalDepth(0, "msg") is the same as Fatal("msg").
func FatalDepth(depth int, args ...interface{}) {
	logging.printDepth(fatalLog, depth, args...)
}

// Fatalln logs to the FATAL, ERROR, WARNING, and INFO logs,
// including a stack trace of all running goroutines, then calls os.Exit(255).
// Arguments are handled in the manner of fmt.Println; a newline is appended if missing.
func Fatalln(args ...interface{}) {
	logging.println(fatalLog, args...)
}

// Fatalf logs to the FATAL, ERROR, WARNING, and INFO logs,
// including a stack trace of all running goroutines, then calls os.Exit(255).
// Arguments are handled in the manner of fmt.Printf; a newline is appended if missing.
func Fatalf(format string, args ...interface{}) {
	logging.printf(fatalLog, format, args...)
}

// FatalfDepth use format and depth print together
func FatalfDepth(depth int, format string, args ...interface{}) {
	logging.printfDepth(fatalLog, depth, format, args...)
}

// fatalNoStacks is non-zero if we are to exit without dumping goroutine stacks.
// It allows Exit and relatives to use the Fatal logs.
var fatalNoStacks uint32

// Exit logs to the FATAL, ERROR, WARNING, and INFO logs, then calls os.Exit(1).
// Arguments are handled in the manner of fmt.Print; a newline is appended if missing.
func Exit(args ...interface{}) {
	atomic.StoreUint32(&fatalNoStacks, 1)
	logging.print(fatalLog, args...)
}

// ExitDepth acts as Exit but uses depth to determine which call frame to log.
// ExitDepth(0, "msg") is the same as Exit("msg").
func ExitDepth(depth int, args ...interface{}) {
	atomic.StoreUint32(&fatalNoStacks, 1)
	logging.printDepth(fatalLog, depth, args...)
}

// Exitln logs to the FATAL, ERROR, WARNING, and INFO logs, then calls os.Exit(1).
func Exitln(args ...interface{}) {
	atomic.StoreUint32(&fatalNoStacks, 1)
	logging.println(fatalLog, args...)
}

// Exitf logs to the FATAL, ERROR, WARNING, and INFO logs, then calls os.Exit(1).
// Arguments are handled in the manner of fmt.Printf; a newline is appended if missing.
func Exitf(format string, args ...interface{}) {
	atomic.StoreUint32(&fatalNoStacks, 1)
	logging.printf(fatalLog, format, args...)
}

func ShrinePureString(str string, sepOut string, sepInner string) string {
	return logging.shrineRequestField(str, sepOut, sepInner)
}

func (l *loggingT) shrineRequestField(str string, sepOut string, sepInner string) (shrineStr string) {
	strs := strings.Split(str, sepOut)
	for index := range strs {
		if strings.Contains(strs[index], sepInner) {
			tmpStrs := strings.Split(strs[index], sepInner)
			switch {
			case strings.Contains(tmpStrs[0], `"num\"`):
				if l.filterIdentity {
					tmpStrs[1] = strMask(tmpStrs[1], 6, 10)
				}
			case strings.Contains(tmpStrs[0], `"num"`):
				if l.filterIdentity {
					tmpStrs[1] = strMask(tmpStrs[1], 5, 10)
				}
			case strings.Contains(tmpStrs[0], "id_card"):
				if l.filterIdentity {
					tmpStrs[1] = ShrineIdentity(tmpStrs[1])
				}
			case strings.Contains(tmpStrs[0], "bankcard"):
				fallthrough
			case strings.Contains(tmpStrs[0], "card_no"):
				if l.filterCard {
					tmpStrs[1] = ShrineCardNo(tmpStrs[1])
				}
			case strings.Contains(tmpStrs[0], "realname"):
				if l.filterRealName {
					tmpStrs[1] = ShrineRealName(tmpStrs[1])
				}
			case strings.Contains(tmpStrs[0], "real_name"):
				if l.filterRealName {
					tmpStrs[1] = ShrineRealName(tmpStrs[1])
				}
			case strings.Contains(tmpStrs[0], "email"):
				if l.filterEmail {
					tmpStrs[1] = ShrineEmail(tmpStrs[1])
				}
			case strings.Contains(tmpStrs[0], "mobile"):
				if l.filterPhone {
					tmpStrs[1] = ShrinePhoneNumber(tmpStrs[1])
				}
			}
			strs[index] = strings.Join(tmpStrs, sepInner)
		}
	}
	shrineStr = strings.Join(strs, sepOut)
	return
}

func ShrineCardNo(cardNo string) (shrineStr string) {
	// 长度 [0,5] 的不作处理
	l := len(cardNo)
	if l <= 5 {
		shrineStr = cardNo
		return
	}
	// 长度 [6, 8] 前两位+后四位显示
	if l <= 8 {
		shrineStr = strMask(cardNo, 2, l-3)
		return
	}
	// 长度 [9, 10] 前两位+后四位显示
	if l <= 10 {
		shrineStr = strMask(cardNo, 2, l-5)
		return
	}
	// 长度 [11, 12] 前两位+后四位显示
	if l <= 12 {
		shrineStr = strMask(cardNo, 3, l-6)
		return
	}
	// 长度 [13, 14] 前两位+后四位显示
	if l <= 14 {
		shrineStr = strMask(cardNo, 4, l-7)
		return
	}
	// 长度 15 以上，前6位+后四位显示
	shrineStr = strMask(cardNo, 6, l-10)
	return
}

func ShrineIdentity(id string) string {
	return strMask(id, 4, 10)
}

func ShrinePhoneNumber(phone string) (shrineStr string) {
	return strMask(phone, 3, 4)
}

func ShrineAlipayAccountNumber(alipayAccountNumber string) (shrineStr string) {
	//支付宝账号是11位手机号
	if isPhoneNumber := IsValidPhoneNumber(alipayAccountNumber); isPhoneNumber {
		shrineStr = ShrinePhoneNumber(alipayAccountNumber)
		return
	}
	//判断是否是邮箱号
	if isEmail := IsEmailRe.MatchString(alipayAccountNumber); isEmail {
		shrineStr = ShrineEmail(alipayAccountNumber)
		return
	}
	//其余类型
	shrineStr = ShrineCardNo(alipayAccountNumber)
	return
}

func ShrineEmail(email string) (shrineStr string) {
	endPos := len(email)
	startPos := strings.Index(email, "@")
	headStr, err := SubString(email, 0, startPos)
	if err != nil {
		Warningf("SubString ShrineEmail headStr faild, email=%s error=%s", email, err)
		shrineStr = ""
		return
	}
	if len(headStr) > 3 {
		headStr, err = SubString(headStr, 0, 3)
		if err != nil {
			Warningf("SubString ShrineEmail headStr faild, phoneNumber=%s headStr=%s error=%s", email, headStr, err)
			shrineStr = ""
			return
		}
		headStr += "***"
	}

	tailStr, err := SubString(email, startPos, endPos)
	if err != nil {
		Errorf("SubString ShrineEmail tailStr faild, phoneNumber=%s error=%s", email, err)
		shrineStr = ""
		return
	}

	shrineStr = headStr + tailStr
	return
}

// ShrineRealName mask real name
func ShrineRealName(realName string) (out string) {
	if len(realName) > 0 {
		if realName[0] >= 0x61 && realName[0] <= 0x7a {

			abroadRealname := strings.Split(realName, " ")
			out = strings.Repeat("*", 2) + abroadRealname[len(abroadRealname)-1]
		} else if realName[0] >= 0x41 && realName[0] <= 0x5a {

			abroadRealname := strings.Split(realName, " ")
			out = strings.Repeat("*", 2) + abroadRealname[len(abroadRealname)-1]
		} else {

			runes := []rune(realName)
			if len(runes) < 2 {
				out = realName
			} else {
				out = strings.Repeat("*", 2) + string(runes[len(runes)-1:])
			}
		}
	}
	return
}

type ShrineFunc func(string) string

func ShrineCommaStr(str string, mShrineType shrinetype) (shrineStr string) {
	var shrineFunc ShrineFunc
	switch mShrineType {
	case shrineCardType:
		shrineFunc = ShrineCardNo
	case shrinePhoneType:
		shrineFunc = ShrinePhoneNumber
	}

	switch {
	case strings.Contains(str, ","):
		tmpStrs := strings.Split(str, ",")
		tmpStrs[1] = shrineFunc(strings.TrimSpace(tmpStrs[1]))
		shrineStr = strings.Join(tmpStrs, ",")
	case strings.Contains(str, "，"):
		tmpStrs := strings.Split(str, "，")
		tmpStrs[1] = shrineFunc(strings.TrimSpace(tmpStrs[1]))
		shrineStr = strings.Join(tmpStrs, "，")
	default:
		shrineStr = str
	}

	return
}

func strMask(number string, offset int, bit int) (out string) {
	b := []rune(number)
	l := len(b)
	if l < offset+1 {
		out = number
		return
	} else {
		if l > offset+bit {
			out = string(b[0:offset]) + strings.Repeat("*", bit) + string(b[offset+bit:])
		} else {
			out = string(b[0:offset]) + strings.Repeat("*", l-offset)
		}
	}
	return
}

//判断手机号码
func IsValidPhoneNumber(phoneNumber string) bool {
	var ok bool = true
	if strings.Contains(phoneNumber, "-") {
		if isPhoneNumberCountry := IsPhoneNumberCountryRe.MatchString(phoneNumber); isPhoneNumberCountry {
			return ok
		}

	} else {
		if isPhoneNumber := IsPhoneNumberRe.MatchString(phoneNumber); isPhoneNumber {
			return ok
		}
	}
	return !ok
}

//截取字符串 start 起点下标 end 终点下标(不包括)
func SubString(str string, start int, end int) (subString string, err error) {
	if end < start {
		err = errors.New("参数不合法")
		return
	}
	rs := []rune(str)
	length := len(rs)
	if start < 0 || start > length {
		err = errors.New("参数不合法")
		return
	}
	if end < 0 || end > length {
		err = errors.New("参数不合法")
		return
	}
	subString = string(rs[start:end])
	return
}

// getStartOfNextTime gets the next time period by time
func getStartOfNextTime(t time.Time) time.Time {
	switch *LogRotateInterval {
	case "month":
		return getStartOfNextMonth(t)
	case "day":
		return getStartOfNextDay(t)
	case "hour":
		return getStartOfNextHour(t)
	case "minute":
		return getStartOfNextMinute(t)
	default:
		return getStartOfNextDay(t)
	}
}

// getStartOfNextMonth start time of next month
func getStartOfNextMonth(t time.Time) time.Time {
	m := t.Month()
	for {
		t = t.AddDate(0, 0, 28)
		if t.Month() != m {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		}
	}
}

// getStartOfNextDay start time of next day
func getStartOfNextDay(t time.Time) time.Time {
	t = t.AddDate(0, 0, 1)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// getStartOfNextHour start time of next hour
func getStartOfNextHour(t time.Time) time.Time {
	t = t.Add(time.Hour)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

// getStartOfNextMinute start time of next minute
func getStartOfNextMinute(t time.Time) time.Time {
	t = t.Add(time.Minute)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, t.Location())
}

// ShrinePwdStr password mask
func ShrinePwdStr() string {
	return "******"
}

// ShrineCompanyName company name mask
func ShrineCompanyName(str string) string {
	if len(str) == 0 {
		return ""
	}
	// Chinese character length of the company
	com := ([]rune)(str)
	comLen := len(com)
	// Less than 3 characters directly return str
	if comLen <= 3 {
		return str
	}
	// Returns the first character + "****" + the last two characters
	return string(com[0]) + "****" + string(com[comLen-2:])
}
