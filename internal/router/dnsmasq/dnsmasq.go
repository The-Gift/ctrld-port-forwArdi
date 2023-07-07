package dnsmasq

import (
	"errors"
	"html/template"
	"net"
	"path/filepath"
	"strings"

	"github.com/Control-D-Inc/ctrld"
)

const ConfigContentTmpl = `# GENERATED BY ctrld - DO NOT MODIFY
no-resolv
{{- range .Upstreams}}
server={{ .Ip }}#{{ .Port }}
{{- end}}
{{- if .SendClientInfo}}
add-mac
{{- end}}
`

const MerlinPostConfPath = "/jffs/scripts/dnsmasq.postconf"
const MerlinPostConfMarker = `# GENERATED BY ctrld - EOF`
const MerlinPostConfTmpl = `# GENERATED BY ctrld - DO NOT MODIFY

#!/bin/sh

config_file="$1"
. /usr/sbin/helper.sh

pid=$(cat /tmp/ctrld.pid 2>/dev/null)
if [ -n "$pid" ] && [ -f "/proc/${pid}/cmdline" ]; then
  pc_delete "servers-file" "$config_file"           # no WAN DNS settings
  pc_append "no-resolv" "$config_file"              # do not read /etc/resolv.conf
  # use ctrld as upstream
  pc_delete "server=" "$config_file"
  {{- range .Upstreams}}
  pc_append "server={{ .Ip }}#{{ .Port }}" "$config_file"
  {{- end}}
  {{- if .SendClientInfo}}
  pc_append "add-mac" "$config_file"                # add client mac
  {{- end}}
  pc_delete "dnssec" "$config_file"                 # disable DNSSEC
  pc_delete "trust-anchor=" "$config_file"          # disable DNSSEC
	
  # For John fork
  pc_delete "resolv-file" "$config_file"            # no WAN DNS settings

  # Change /etc/resolv.conf, which may be changed by WAN DNS setup
  pc_delete "nameserver" /etc/resolv.conf
  pc_append "nameserver 127.0.0.1" /etc/resolv.conf

  exit 0
fi
`

type Upstream struct {
	Ip   string
	Port int
}

func ConfTmpl(tmplText string, cfg *ctrld.Config) (string, error) {
	listener := cfg.FirstListener()
	if listener == nil {
		return "", errors.New("missing listener")
	}
	ip := listener.IP
	if ip == "0.0.0.0" || ip == "::" || ip == "" {
		ip = "127.0.0.1"
	}
	upstreams := []Upstream{{Ip: ip, Port: listener.Port}}
	return confTmpl(tmplText, upstreams, cfg.HasUpstreamSendClientInfo())
}

func FirewallaConfTmpl(tmplText string, cfg *ctrld.Config) (string, error) {
	if lc := cfg.FirstListener(); lc != nil && (lc.IP == "0.0.0.0" || lc.IP == "") {
		return confTmpl(tmplText, firewallaUpstreams(lc.Port), cfg.HasUpstreamSendClientInfo())
	}
	return ConfTmpl(tmplText, cfg)
}

func confTmpl(tmplText string, upstreams []Upstream, sendClientInfo bool) (string, error) {
	tmpl := template.Must(template.New("").Parse(tmplText))
	var to = &struct {
		SendClientInfo bool
		Upstreams      []Upstream
	}{
		SendClientInfo: sendClientInfo,
		Upstreams:      upstreams,
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, to); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func firewallaUpstreams(port int) []Upstream {
	matches, err := filepath.Glob("/home/pi/firerouter/etc/dnsmasq.dns.*.conf")
	if err != nil {
		return nil
	}
	upstreams := make([]Upstream, 0, len(matches))
	for _, match := range matches {
		// Trim prefix and suffix to get the iface name only.
		ifaceName := strings.TrimSuffix(strings.TrimPrefix(match, "/home/pi/firerouter/etc/dnsmasq.dns."), ".conf")
		if netIface, _ := net.InterfaceByName(ifaceName); netIface != nil {
			addrs, _ := netIface.Addrs()
			for _, addr := range addrs {
				if netIP, ok := addr.(*net.IPNet); ok && netIP.IP.To4() != nil {
					upstreams = append(upstreams, Upstream{
						Ip:   netIP.IP.To4().String(),
						Port: port,
					})
				}
			}
		}
	}
	return upstreams
}
