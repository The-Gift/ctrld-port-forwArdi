package router

import (
	"errors"
	"fmt"
	"os/exec"
)

const (
	nvramCtrldKeyPrefix  = "ctrld_"
	nvramCtrldSetupKey   = "ctrld_setup"
	nvramCtrldInstallKey = "ctrld_install"
	nvramRCStartupKey    = "rc_startup"
)

//lint:ignore ST1005 This error is for human.
var errDdwrtJffs2NotEnabled = errors.New(`could not install service without jffs, follow this guide to enable:

https://wiki.dd-wrt.com/wiki/index.php/Journalling_Flash_File_System
`)

func setupDDWrt() error {
	// Already setup.
	if val, _ := nvram("get", nvramCtrldSetupKey); val == "1" {
		return nil
	}

	data, err := dnsMasqConf()
	if err != nil {
		return err
	}

	nvramKvMap := nvramSetupKV()
	nvramKvMap["dnsmasq_options"] = data
	if err := nvramSetKV(nvramKvMap, nvramCtrldSetupKey); err != nil {
		return err
	}

	// Restart dnsmasq service.
	if err := restartDNSMasq(); err != nil {
		return err
	}
	return nil
}

func cleanupDDWrt() error {
	// Restore old configs.
	if err := nvramRestore(nvramSetupKV(), nvramCtrldSetupKey); err != nil {
		return err
	}
	// Restart dnsmasq service.
	if err := restartDNSMasq(); err != nil {
		return err
	}
	return nil
}

func postInstallDDWrt() error {
	return nil
}

func ddwrtRestartDNSMasq() error {
	if out, err := exec.Command("restart_dns").CombinedOutput(); err != nil {
		return fmt.Errorf("restart_dns: %s, %w", string(out), err)
	}
	return nil
}

func ddwrtJff2Enabled() bool {
	out, _ := nvram("get", "enable_jffs2")
	return out == "1"
}
