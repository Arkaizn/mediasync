// Package lyrics parses LRC lyrics and fetches lyrics from LRCLIB.
package lyrics

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/arkaizn/mediasync/internal/model"
)

var (
	timeTag   = regexp.MustCompile(`\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)
	metaTag   = regexp.MustCompile(`^\[([A-Za-z#]+):(.*)\]\s*$`)
	wordTimes = regexp.MustCompile(`<\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?>`)
)

// ParseLRC parses LRC text. Lines with time tags become synced lines (a line
// with several tags is repeated for each); with no time tags at all, every
// line is an unsynced line. The [offset:±ms] tag is applied (positive offset
// shows lyrics earlier). Metadata tags are dropped; enhanced word timings are
// stripped. synced reports whether any time tag was found.
func ParseLRC(text string) (lines []model.LyricLine, synced bool) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var offset int64
	var timed []model.LyricLine
	var plain []model.LyricLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t")
		if m := metaTag.FindStringSubmatch(line); m != nil && !timeTag.MatchString(line) {
			if strings.EqualFold(m[1], "offset") {
				if v, err := strconv.ParseInt(strings.TrimSpace(m[2]), 10, 64); err == nil {
					offset = v
				}
			}
			continue
		}
		tags := timeTag.FindAllStringSubmatchIndex(line, -1)
		if len(tags) == 0 || tags[0][0] != 0 {
			plain = append(plain, model.LyricLine{Text: strings.TrimSpace(wordTimes.ReplaceAllString(line, ""))})
			continue
		}
		// Leading consecutive time tags apply to the rest of the line.
		end := 0
		var starts []int64
		for _, t := range tags {
			if t[0] != end {
				break
			}
			end = t[1]
			starts = append(starts, tagMS(line, t))
		}
		body := strings.TrimSpace(wordTimes.ReplaceAllString(line[end:], ""))
		for _, st := range starts {
			v := st
			timed = append(timed, model.LyricLine{StartMS: &v, Text: body})
		}
	}
	if len(timed) > 0 {
		for i := range timed {
			v := max(*timed[i].StartMS-offset, 0)
			timed[i].StartMS = &v
		}
		sort.SliceStable(timed, func(i, j int) bool { return *timed[i].StartMS < *timed[j].StartMS })
		return timed, true
	}
	return TrimLines(plain), false
}

func tagMS(line string, t []int) int64 {
	min, _ := strconv.ParseInt(line[t[2]:t[3]], 10, 64)
	sec, _ := strconv.ParseInt(line[t[4]:t[5]], 10, 64)
	ms := min*60_000 + sec*1000
	if t[6] >= 0 {
		frac := line[t[6]:t[7]]
		v, _ := strconv.ParseInt(frac, 10, 64)
		switch len(frac) {
		case 1:
			v *= 100
		case 2:
			v *= 10
		}
		ms += v
	}
	return ms
}

// TrimLines removes leading and trailing empty lines.
func TrimLines(lines []model.LyricLine) []model.LyricLine {
	for len(lines) > 0 && strings.TrimSpace(lines[0].Text) == "" && lines[0].StartMS == nil {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1].Text) == "" && lines[len(lines)-1].StartMS == nil {
		lines = lines[:len(lines)-1]
	}
	if lines == nil {
		return []model.LyricLine{}
	}
	return lines
}
