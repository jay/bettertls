// Copyright 2017 Google, Inc.
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

// go_x509 tests the Go certificate verification against the test cases. Note
// that, since IP-address name constraints aren't supported in Go, they are not
// tested here. (Go will reject any certifciate with critical, IP constraints.)
package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// baseDir is the path to the top of the bettertls repo.
const baseDir = ".."

// configFile represents config.json in the top-level of the repo.
type configFile struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
}

// expectations represents expects.json, which is generated by
// defineExpects.js.
type expectations struct {
	Expects []expectation
}

type expectation struct {
	Id           int            `json:"id"`
	IP           expectedResult `json:"ip"`
	DNS          expectedResult `json:"dns"`
	Descriptions []string       `json:"descriptions"`

	// testDNS is not part of expects.json but, here, indicates whether the
	// IP or DNS behaviour should be tested.
	testDNS bool
	// err is also not part of expects.json but, here, contains the error
	// resulting from running the test.
	err error
}

func (e *expectation) descriptions() []string {
	var ret []string
	ret = append(ret, e.Descriptions...)

	if e.testDNS {
		ret = append(ret, e.DNS.Descriptions...)
	} else {
		ret = append(ret, e.IP.Descriptions...)
	}

	return ret
}

type expectedResult struct {
	Result       string   `json:"expect"`
	Descriptions []string `json:"descriptions"`
}

// runTests runs all tests and returns nil on success.
func runTests() error {
	root, err := loadRoot()
	if err != nil {
		return err
	}

	config, err := loadConfig()
	if err != nil {
		return err
	}

	expectations, err := loadExpectations()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	numWorkers := runtime.NumCPU() * 2
	work := make(chan expectation, numWorkers)
	failures := make(chan expectation, numWorkers)
	failureCount := make(chan int)

	for i := 0; i < numWorkers; i++ {
		go worker(failures, work, &wg, config, root)
		wg.Add(1)
	}

	go failureCounter(failureCount, failures)

	for _, expectation := range expectations.Expects {
		// Each test is run twice, once to test verifying against the
		// DNS name and again to test verifying against the IP address.
		// (Although Go doesn't support the latter so they're discarded
		// later.)
		expectation.testDNS = false
		work <- expectation
		expectation.testDNS = true
		work <- expectation
	}

	close(work)
	wg.Wait()
	close(failures)

	numFailures := <-failureCount
	if numFailures != 0 {
		return fmt.Errorf("failed %d of %d tests", numFailures, len(expectations.Expects))
	}

	return nil
}

// worker reads tests from work and writes any failures to failures.
func worker(failures chan<- expectation, work <-chan expectation, wg *sync.WaitGroup, config *configFile, root *x509.Certificate) {
	defer wg.Done()

	// These are the description strings that identify why a result is
	// marked as "WEAK-OK".
	const (
		cnWithSANs                = "The DNS name for this certificate exists in the common name but not in the Subject Alternate Names extension even though the extension is specified. Most implementations will fail DNS-hostname validation on this certificate."
		dnsInCNViolation          = "The DNS name in the common name violates a name constraint. Because there is a SAN extension, this might be ignored."
		forbiddenIPAddressPresent = "Althought the IP address is not the subject name in question, it's name constraint violation may still cause this certificate to be rejected."
		ipInCNViolation           = "The IP in the common name violates a name constraint. Because there is a SAN extension, this might be ignored."
		ipViolation               = "The IP in the SAN extension violates a name constraint."
		noIPGiven                 = "There is a IP name constraint but no IP in the certificate. This isn't an explicit violation, but some implementations will fail to validate the certificate."
	)

	rootPool := x509.NewCertPool()
	rootPool.AddCert(root)

NextTest:
	for test := range work {
		if !test.testDNS {
			// Go doesn't support verifying against an IP address.
			continue
		}

		chain, err := readPEMChain(filepath.Join(baseDir, "certificates", strconv.Itoa(test.Id)+".chain"))
		if err != nil {
			test.err = err
			failures <- test
			continue
		}

		leaf, err := readPEMChain(filepath.Join(baseDir, "certificates", strconv.Itoa(test.Id)+".crt"))
		if err != nil {
			test.err = err
			failures <- test
			continue
		}

		if len(leaf) != 1 {
			test.err = fmt.Errorf("expected a single certificate in the .crt file, but found %d", len(leaf))
			failures <- test
			continue
		}

		intermediatePool := x509.NewCertPool()
		for _, intermediate := range chain {
			intermediatePool.AddCert(intermediate)
		}

		verifyOpts := x509.VerifyOptions{
			Roots:         rootPool,
			Intermediates: intermediatePool,
			DNSName:       config.Hostname,
		}

		var shouldFail bool
		switch test.DNS.Result {
		default:
			test.err = fmt.Errorf("unknown expected result %q", test.DNS.Result)
			failures <- test
			continue
		case "ERROR":
			shouldFail = true
		case "OK":
			shouldFail = false
		case "WEAK-OK":
			descriptions := test.descriptions()
			if len(descriptions) == 0 {
				test.err = errors.New("Weak-OK without description")
				failures <- test
				continue
			}

		Descriptions:
			for _, desc := range descriptions {
				switch desc {
				case forbiddenIPAddressPresent, noIPGiven, ipViolation, ipInCNViolation, dnsInCNViolation:
					shouldFail = false

				case cnWithSANs:
					// Any description that should be fatal
					// means that a failure must occur.
					shouldFail = true
					break Descriptions

				default:
					test.err = fmt.Errorf("unknown description for weak-OK: %q", desc)
					failures <- test
					continue NextTest
				}
			}
		}

		_, err = leaf[0].Verify(verifyOpts)
		if shouldFail {
			if err == nil {
				failures <- test
			}
		} else {
			if err != nil {
				test.err = err
				failures <- test
			}
		}
	}
}

// failureCounter prints received failures and, once complete, sends the number
// of failures to count.
func failureCounter(count chan<- int, failures <-chan expectation) {
	num := 0

	for failure := range failures {
		num++

		testType := "IP"
		if failure.testDNS {
			testType = "DNS"
		}

		fmt.Printf("#%d: failed for %s:\n  %q\n  %q\n", failure.Id, testType, failure.err, strings.Join(failure.descriptions(), " "))
	}

	count <- num
}

func main() {
	if err := runTests(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
		return
	}

	println("PASS")
}

func loadRoot() (*x509.Certificate, error) {
	rootChain, err := readPEMChain(filepath.Join(baseDir, "certificates", "root.crt"))
	if err != nil {
		return nil, err
	}

	if len(rootChain) != 1 {
		return nil, fmt.Errorf("Expected a single root in 'root.pem' but found %d", len(rootChain))
	}

	return rootChain[0], nil
}

func loadConfig() (*configFile, error) {
	configBytes, err := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	if err != nil {
		return nil, err
	}

	ret := new(configFile)
	if err := json.Unmarshal(configBytes, &ret); err != nil {
		return nil, err
	}

	return ret, nil
}

func loadExpectations() (*expectations, error) {
	expectsBytes, err := ioutil.ReadFile(filepath.Join(baseDir, "html", "expects.json"))
	if err != nil {
		return nil, err
	}

	ret := new(expectations)
	if err := json.Unmarshal(expectsBytes, &ret); err != nil {
		return nil, err
	}

	return ret, nil
}

func readPEMChain(path string) (certs []*x509.Certificate, err error) {
	pemBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			break
		}
		pemBytes = rest

		if block.Type != "CERTIFICATE" {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		certs = append(certs, cert)
	}

	return certs, nil
}