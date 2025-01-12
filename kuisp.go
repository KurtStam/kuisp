// KUISP - A utility to serve static content & reverse proxy to RESTful services
//
// Copyright 2015 Red Hat, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gorilla/handlers"
	flag "github.com/spf13/pflag"
	"golang.org/x/net/http2"
)

type Options struct {
	Port                  int
	StaticDir             string
	StaticPrefix          string
	DefaultPage           string
	StaticCacheMaxAge     time.Duration
	Services              services
	FailOnUnknownServices bool
	Configs               configs
	CACerts               caCerts
	SkipCertValidation    bool
	TlsCertFile           string
	TlsKeyFile            string
	AccessLogging         bool
	CompressHandler       bool
	ServeWww              bool
}

var options = &Options{}

func initFlags() {
	flag.IntVarP(&options.Port, "port", "p", 80, "The port to listen on")
	flag.StringVarP(&options.StaticDir, "www", "w", ".", "Directory to serve static files from")
	flag.StringVar(&options.StaticPrefix, "www-prefix", "/", "Prefix to serve static files on")
	flag.DurationVar(&options.StaticCacheMaxAge, "max-age", 0, "Set the Cache-Control header for static content with the max-age set to this value, e.g. 24h. Must confirm to http://golang.org/pkg/time/#ParseDuration")
	flag.StringVarP(&options.DefaultPage, "default-page", "d", "", "Default page to send if page not found")
	flag.VarP(&options.Services, "service", "s", "The Kubernetes services to proxy to in the form \"<prefix>=<serviceUrl>\"")
	flag.VarP(&options.Configs, "config-file", "c", "The configuration files to create in the form \"<template>=<output>\"")
	flag.Var(&options.CACerts, "ca-cert", "CA certs used to verify proxied server certificates")
	flag.StringVar(&options.TlsCertFile, "tls-cert", "", "Certificate file to use to serve using TLS")
	flag.StringVar(&options.TlsKeyFile, "tls-key", "", "Certificate file to use to serve using TLS")
	flag.BoolVar(&options.SkipCertValidation, "skip-cert-validation", false, "Skip remote certificate validation - dangerous!")
	flag.BoolVarP(&options.AccessLogging, "access-logging", "l", false, "Enable access logging")
	flag.BoolVar(&options.CompressHandler, "compress", false, "Enable gzip/deflate response compression")
	flag.BoolVar(&options.FailOnUnknownServices, "fail-on-unknown-services", false, "Fail on unknown services in DNS")
	flag.BoolVar(&options.ServeWww, "serve-www", true, "Whether to serve static content")
	flag.Parse()
}

func main() {
	initFlags()

	if len(options.Configs) > 0 {
		for _, configDef := range options.Configs {
			log.Printf("Creating config file:  %v => %v\n", configDef.template, configDef.output)
			createConfig(configDef.template, configDef.output)
		}
		log.Println()
	}

	if len(options.Services) > 0 {
		tlsConfig := &tls.Config{
			RootCAs:            x509.NewCertPool(),
			InsecureSkipVerify: options.SkipCertValidation,
		}
		transport := &http.Transport{TLSClientConfig: tlsConfig}
		if len(options.CACerts) > 0 {
			for _, caFile := range options.CACerts {
				// Load our trusted certificate path
				pemData, err := ioutil.ReadFile(caFile)
				if err != nil {
					log.Fatal("Couldn't read CA file, ", caFile, ": ", err)
				}
				if ok := tlsConfig.RootCAs.AppendCertsFromPEM(pemData); !ok {
					log.Fatal("Couldn't load PEM data from CA file, ", caFile)
				}
			}
		}
		for _, serviceDef := range options.Services {
			actualHost, port, err := validateServiceHost(serviceDef.url.Host)
			if err != nil {
				if options.FailOnUnknownServices {
					log.Fatalf("Unknown service host: %s", serviceDef.url.Host)
				} else {
					log.Printf("Unknown service host: %s", serviceDef.url.Host)
				}
			} else {
				if len(port) > 0 {
					actualHost += ":" + port
				}
				serviceDef.url.Host = actualHost
			}
			log.Printf("Creating service proxy: %v => %v\n", serviceDef.prefix, serviceDef.url.String())
			rp := httputil.NewSingleHostReverseProxy(serviceDef.url)
			rp.Transport = transport
			http.Handle(serviceDef.prefix, http.StripPrefix(serviceDef.prefix, rp))
		}
		log.Println()
	}

	if options.ServeWww {
		httpDir := http.Dir(options.StaticDir)
		staticHandler := http.FileServer(httpDir)
		if options.StaticCacheMaxAge > 0 {
			staticHandler = maxAgeHandler(options.StaticCacheMaxAge.Seconds(), staticHandler)
		}

		if len(options.DefaultPage) > 0 {
			staticHandler = defaultPageHandler(options.DefaultPage, httpDir, staticHandler)
		}
		if options.CompressHandler {
			staticHandler = handlers.CompressHandler(staticHandler)
		}
		http.Handle(options.StaticPrefix, staticHandler)
	}

	log.Printf("Listening on :%d\n", options.Port)
	log.Println()

	registerMimeTypes()

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", options.Port),
	}
	http2.ConfigureServer(srv, &http2.Server{})

	var handler http.Handler = http.DefaultServeMux

	if options.AccessLogging {
		handler = handlers.CombinedLoggingHandler(os.Stdout, handler)
	}

	srv.Handler = handler

	if len(options.TlsCertFile) > 0 && len(options.TlsKeyFile) > 0 {
		log.Fatal(srv.ListenAndServeTLS(options.TlsCertFile, options.TlsKeyFile))
	} else {
		log.Fatal(srv.ListenAndServe())
	}
}

func defaultPageHandler(defaultPage string, httpDir http.Dir, fsHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := httpDir.Open(r.URL.Path); err != nil {
			splitPath := strings.Split(r.URL.Path, "/")
			for {
				p := append(splitPath, defaultPage)
				dp := path.Join(p...)
				if defaultFile, err := httpDir.Open(dp); err == nil {
					if stat, err := defaultFile.Stat(); err == nil {
						http.ServeContent(w, r, stat.Name(), stat.ModTime(), defaultFile)
						return
					}
				}
				if len(splitPath) == 0 {
					http.NotFound(w, r)
					return
				}
				splitPath = splitPath[:len(splitPath)-1]
			}
		} else {
			fsHandler.ServeHTTP(w, r)
		}
	})
}

func maxAgeHandler(seconds float64, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", fmt.Sprintf("max-age=%g, public, must-revalidate, proxy-revalidate", seconds))
		h.ServeHTTP(w, r)
	})
}

func validateServiceHost(host string) (string, string, error) {
	actualHost, port, err := net.SplitHostPort(host)
	if err != nil {
		actualHost = host
	}
	if ip := net.ParseIP(actualHost); ip != nil {
		return actualHost, port, nil
	}
	_, err = net.LookupIP(actualHost)
	if err != nil {
		if !strings.Contains(actualHost, ".") {
			actualHost = strings.ToUpper(actualHost)
			actualHost = strings.Replace(actualHost, "-", "_", -1)
			serviceHostEnvVar := os.Getenv(actualHost + "_SERVICE_HOST")
			if net.ParseIP(serviceHostEnvVar) != nil {
				return serviceHostEnvVar, os.Getenv(actualHost + "_SERVICE_PORT"), nil
			}
		}
		return "", "", err
	}
	return actualHost, port, nil
}
