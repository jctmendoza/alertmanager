// Copyright 2015 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"database/sql"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/prometheus/common/log"
	"github.com/prometheus/common/route"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/template"
)

var (
	configFile    = flag.String("config.file", "config.yml", "The configuration file")
	dataDir       = flag.String("data.dir", "data/", "The data directory")
	listenAddress = flag.String("web.listen-address", ":9093", "Address to listen on for the web interface and API.")
)

func main() {
	flag.Parse()

	db, err := sql.Open("ql", filepath.Join(*dataDir, "am.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	alerts, err := provider.NewSQLAlerts(db)
	if err != nil {
		log.Fatal(err)
	}
	notifies, err := provider.NewSQLNotifyInfo(db)
	if err != nil {
		log.Fatal(err)
	}
	silences, err := provider.NewSQLSilences(db)
	if err != nil {
		log.Fatal(err)
	}

	var (
		inhibitor *Inhibitor
		tmpl      *template.Template
		disp      *Dispatcher
	)
	defer disp.Stop()

	build := func(nconf []*config.NotificationConfig) notify.Notifier {
		var (
			router  = notify.Router{}
			fanouts = notify.Build(nconf, tmpl)
		)
		for name, fo := range fanouts {
			for i, n := range fo {
				n = notify.Retry(n)
				n = notify.Log(n, log.With("step", "retry"))
				n = notify.Dedup(notifies, n)
				n = notify.Log(n, log.With("step", "dedup"))

				fo[i] = n
			}
			router[name] = fo
		}
		var n notify.Notifier = router

		n = notify.Log(n, log.With("step", "route"))
		n = notify.Mute(silences, n)
		n = notify.Log(n, log.With("step", "silence"))
		n = notify.Mute(inhibitor, n)
		n = notify.Log(n, log.With("step", "inhibit"))

		return n
	}

	reload := func() (err error) {
		log.With("file", *configFile).Infof("Loading configuration file")
		defer func() {
			if err != nil {
				log.With("file", *configFile).Errorf("Loading configuration file failed")
			}
		}()

		conf, err := config.LoadFile(*configFile)
		if err != nil {
			return err
		}

		tmpl, err = template.FromGlobs(conf.Templates...)
		if err != nil {
			return err
		}

		disp.Stop()

		inhibitor = NewInhibitor(alerts, conf.InhibitRules)
		disp = NewDispatcher(alerts, conf.Routes, build(conf.NotificationConfigs))

		go disp.Run()

		return nil
	}

	if err := reload(); err != nil {
		os.Exit(1)
	}

	router := route.New()
	NewAPI(router.WithPrefix("/api/v1"), alerts, silences)

	go http.ListenAndServe(*listenAddress, router)

	var (
		hup  = make(chan os.Signal)
		term = make(chan os.Signal)
	)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	go func() {
		for range hup {
			reload()
		}
	}()

	<-term

	log.Infoln("Received SIGTERM, exiting gracefully...")
}
