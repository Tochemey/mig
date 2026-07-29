// MIT License
//
// Copyright (c) 2026 Arsene Tochemey Gandote
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//

// Command dummy is the executable wrapper around package dummy, spawned by the
// harness's own tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tochemey/mig/test/dummy"
)

func main() {
	dsn := flag.String("dsn", "", "postgres connection string")
	hold := flag.Duration("hold", 10*time.Minute, "how long to keep the backend busy")
	flag.Parse()

	ready := func(marker string) {
		fmt.Println(marker)
	}

	if err := dummy.Run(context.Background(), *dsn, *hold, ready); err != nil {
		fmt.Fprintf(os.Stderr, "dummy: %v\n", err)
		os.Exit(1)
	}
}
