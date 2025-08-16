package utils

import (
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func EscapeMd(s string) string {
	repl := []string{"*", "\\*", "_", "\\_", "`", "\\`", "~", "\\~"}
	r := strings.NewReplacer(repl...)
	return r.Replace(s)
}

func PrettyTime(sec int) string {
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

var reDur = regexp.MustCompile(`(?i)^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

func ParseDurationString(s string) int {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	m := reDur.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	h := Atoi(m[1])
	min := Atoi(m[2])
	sec := Atoi(m[3])
	return h*3600 + min*60 + sec
}

func Atoi(s string) int {
	if s == "" {
		return 0
	}
	v, _ := strconv.Atoi(s)
	return v
}

func ShuffleSlice[T any](a []T) {
	var seed int64
	_ = binaryReadRand(&seed) // falls back to time-based if needed
	r := mrand.New(mrand.NewSource(seed))
	r.Shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
}

// binaryReadRand reads 8 random bytes into an int64 seed.
func binaryReadRand(dst *int64) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		*dst = time.Now().UnixNano()
		return err
	}
	*dst = int64(uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]))
	return nil
}
