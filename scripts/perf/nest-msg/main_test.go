package main

import (
	"testing"
	"time"
)

func TestLoadFixtureCountsRealCompletions(t *testing.T) {
	for _, mode := range []string{"dispatch", "request"} {
		for _, distribution := range []string{"hot", "spread"} {
			t.Run(mode+"/"+distribution, func(t *testing.T) {
				c := config{Mode: mode, Distribution: distribution, Messages: 2000, Warmup: 64, Entities: 100, Workers: 2, Producers: 4, Window: 4, Queue: 128, SampleEvery: 16, Timeout: 10 * time.Second}
				result, err := run(c)
				if err != nil {
					t.Fatal(err)
				}
				if !result.Correct || result.Completed != 2000 || result.Processed != 2000 || result.LatencySamples != 128 {
					t.Fatalf("invalid completed-message accounting: %+v", result)
				}
				if result.P99US < 0 || result.P99US > result.MaxSampleUS {
					t.Fatalf("invalid latency samples: %+v", result)
				}
			})
		}
	}
}
