package transcode

import (
	"fmt"
	"math"
	"strings"
)

// minTailSec: a remainder shorter than this is folded into the previous
// segment rather than becoming a one-frame segment of its own.
const minTailSec = 0.5

// segmentCount is how many segments a duration yields at segSec each; the
// last one takes the remainder (up to segSec+minTailSec). Zero when the
// duration is unknown.
func segmentCount(durationSec, segSec float64) int {
	if durationSec <= 0 || segSec <= 0 {
		return 0
	}
	n := int(math.Ceil((durationSec-minTailSec)/segSec - 1e-9))
	if n < 1 {
		n = 1
	}
	return n
}

// segmentName is the bare relative URI of segment n ("seg-00001.ts"). The SPA
// rewrites these for the PHP bridge, so they must stay bare.
func segmentName(n int) string { return fmt.Sprintf("seg-%05d.ts", n) }

// playlist renders the VOD playlist for a session. Every EXTINF is the
// nominal segment length (the last one the remainder); in remux mode the real
// segment boundaries sit on keyframes and may differ by up to one segment,
// which hls.js corrects from the segments' own timestamps.
func playlist(durationSec, segSec float64, count int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	durs := make([]float64, count)
	longest := 0.0
	for n := range durs {
		d := segSec
		if n == count-1 {
			d = durationSec - float64(n)*segSec
		}
		durs[n] = d
		longest = math.Max(longest, d)
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(longest-1e-9)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for n, d := range durs {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", d, segmentName(n))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
