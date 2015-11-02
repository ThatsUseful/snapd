// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2015 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package snappy

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ubuntu-core/snappy/dirs"
	"github.com/ubuntu-core/snappy/helpers"
	"github.com/ubuntu-core/snappy/logger"
	"github.com/ubuntu-core/snappy/pkg"
	"github.com/ubuntu-core/snappy/policy"
)

type errPolicyNotFound struct {
	polType string
	pol     string
}

func (e *errPolicyNotFound) Error() string {
	return fmt.Sprintf("could not find specified %s: %s", e.polType, e.pol)
}

var (
	// Note: these are true for ubuntu-core but perhaps not other flavors
	defaultTemplate     = "default"
	defaultPolicyGroups = []string{"network-client"}

	// These are set elsewhere
	defaultPolicyVendor  = ""
	defaultPolicyVersion = ""

	// Templates and policy groups (caps)
	aaPolicyDir = "/usr/share/apparmor/easyprof"
	scPolicyDir = "/usr/share/seccomp"
	// Framework policy
	aaFrameworkPolicyDir = filepath.Join(policy.SecBase, "apparmor")
	scFrameworkPolicyDir = filepath.Join(policy.SecBase, "seccomp")
	// AppArmor cache dir
	aaCacheDir = "/var/cache/apparmor"

	errOriginNotFound     = errors.New("could not detect origin")
	errPolicyTypeNotFound = errors.New("could not find specified policy type")
	errInvalidAppID       = errors.New("invalid APP_ID")
	errPolicyGen          = errors.New("errors found when generating policy")

	// ErrSystemVersionNotFound could not detect system version (eg, 15.04,
	// 15.10, etc)
	ErrSystemVersionNotFound = errors.New("could not detect system version")
	// ErrSystemFlavorNotFound could not detect system flavor (eg,
	// ubuntu-core, ubuntu-personal, etc)
	ErrSystemFlavorNotFound = errors.New("could not detect system flavor")
)

var (
	lsbRelease        = "/etc/lsb-release"
	runAppArmorParser = runAppArmorParserImpl
)

func runAppArmorParserImpl(argv ...string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	return cmd.CombinedOutput()
}

// SecuritySeccompOverrideDefinition is used to override seccomp security
// defaults
type SecuritySeccompOverrideDefinition struct {
	Syscalls []string `yaml:"syscalls,omitempty" json:"syscalls,omitempty"`
}

// SecurityAppArmorOverrideDefinition is used to override apparmor security
// defaults
type SecurityAppArmorOverrideDefinition struct {
	ReadPaths    []string `yaml:"read-paths,omitempty" json:"read-paths,omitempty"`
	WritePaths   []string `yaml:"write-paths,omitempty" json:"write-paths,omitempty"`
	Abstractions []string `yaml:"abstractions,omitempty" json:"abstractions,omitempty"`
}

// SecurityOverrideDefinition is used to override apparmor or seccomp
// security defaults
type SecurityOverrideDefinition struct {
	AppArmor *SecurityAppArmorOverrideDefinition `yaml:"apparmor" json:"apparmor"`
	Seccomp  *SecuritySeccompOverrideDefinition  `yaml:"seccomp" json:"seccomp"`
}

// SecurityPolicyDefinition is used to provide hand-crafted policy
type SecurityPolicyDefinition struct {
	AppArmor string `yaml:"apparmor" json:"apparmor"`
	Seccomp  string `yaml:"seccomp" json:"seccomp"`
}

// SecurityDefinitions contains the common apparmor/seccomp definitions
type SecurityDefinitions struct {
	// SecurityTemplate is a template like "default"
	SecurityTemplate string `yaml:"security-template,omitempty" json:"security-template,omitempty"`
	// SecurityOverride is a override for the high level security json
	SecurityOverride *SecurityOverrideDefinition `yaml:"security-override,omitempty" json:"security-override,omitempty"`
	// SecurityPolicy is a hand-crafted low-level policy
	SecurityPolicy *SecurityPolicyDefinition `yaml:"security-policy,omitempty" json:"security-policy,omitempty"`

	// SecurityCaps is are the apparmor/seccomp capabilities for an app
	SecurityCaps []string `yaml:"caps,omitempty" json:"caps,omitempty"`
}

type securityAppID struct {
	AppID   string
	Pkgname string
	Appname string
	Version string
}

// findUbuntuFlavor determines the flavor (eg, ubuntu-core, ubuntu-personal,
// etc) of the system, which is needed for determining the security policy
// policy-vendor
func findUbuntuFlavor() (string, error) {
	// TODO: a downloaded snap targets a particular device. We need to map
	// that device type (flavor) to installed system security policy (eg
	// ubuntu-core, ubuntu-personal, etc). As of 2015-10-28,
	// ubuntu-personal images are no longer generated, and there is no
	// mechanism to know what the snap targets.
	//
	// FIXME: just use:
	//    return fmt.Sprintf("ubuntu-%s", release.Get().Flavor)
	// here
	return "ubuntu-core", nil
}

// findUbuntuVersion determines the version (eg, 15.04, 15.10, etc) of the
// system, which is needed for determining the security policy
// policy-version
func findUbuntuVersion() (string, error) {
	content, err := ioutil.ReadFile(lsbRelease)
	if err != nil {
		logger.Noticef("Failed to read %q: %v", lsbRelease, err)
		return "", err
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "DISTRIB_RELEASE=") {
			tmp := strings.Split(line, "=")
			if len(tmp) != 2 {
				return "", ErrSystemVersionNotFound
			}
			return tmp[1], nil
		}
	}

	return "", ErrSystemVersionNotFound
}

// Generate a string suitable for use in a DBus object
func dbusPath(s string) string {
	dbusStr := ""
	ok := regexp.MustCompile(`^[a-zA-Z0-9]$`)

	for _, c := range strings.SplitAfter(s, "") {
		if ok.MatchString(c) {
			dbusStr += c
		} else {
			dbusStr += fmt.Sprintf("_%02x", c)
		}
	}

	return dbusStr
}

// Calculate whitespace prefix based on occurrence of s in t
func findWhitespacePrefix(t string, s string) string {
	pat := regexp.MustCompile(`^ *` + s)
	p := ""
	for _, line := range strings.Split(t, "\n") {
		if pat.MatchString(line) {
			for i := 0; i < len(line)-len(strings.TrimLeft(line, " ")); i++ {
				p += " "
			}
			break
		}
	}
	return p
}

func getSecurityProfile(m *packageYaml, appName, baseDir string) (string, error) {
	cleanedName := strings.Replace(appName, "/", "-", -1)
	if m.Type == pkg.TypeFramework || m.Type == pkg.TypeOem {
		return fmt.Sprintf("%s_%s_%s", m.Name, cleanedName, m.Version), nil
	}

	origin, err := originFromYamlPath(filepath.Join(baseDir, "meta", "package.yaml"))

	return fmt.Sprintf("%s.%s_%s_%s", m.Name, origin, cleanedName, m.Version), err
}

func findTemplate(template string, policyType string) (string, error) {
	if template == "" {
		template = defaultTemplate
	}

	systemTemplate := ""
	fwTemplate := ""
	subdir := filepath.Join("templates", defaultPolicyVendor, defaultPolicyVersion)
	if policyType == "apparmor" {
		systemTemplate = filepath.Join(aaPolicyDir, subdir, template)
		fwTemplate = filepath.Join(aaFrameworkPolicyDir, "templates", template)
	} else if policyType == "seccomp" {
		systemTemplate = filepath.Join(scPolicyDir, subdir, template)
		fwTemplate = filepath.Join(scFrameworkPolicyDir, "templates", template)
	} else {
		return "", errPolicyTypeNotFound
	}

	// Always prefer system policy
	fns := []string{systemTemplate, fwTemplate}
	for _, fn := range fns {
		content, err := ioutil.ReadFile(fn)
		if err == nil {
			return string(content), nil
		}
	}

	return "", &errPolicyNotFound{"template", template}
}

func findCaps(caps []string, template string, policyType string) (string, error) {
	// XXX: this is snappy specific, on other systems like the phone we may
	// want different defaults.
	if template == "" && caps == nil {
		caps = defaultPolicyGroups
	} else if caps == nil {
		caps = []string{}
	}

	subdir := filepath.Join("policygroups", defaultPolicyVendor, defaultPolicyVersion)
	parent := ""
	fwParent := ""
	if policyType == "apparmor" {
		parent = filepath.Join(aaPolicyDir, subdir)
		fwParent = filepath.Join(aaFrameworkPolicyDir, "policygroups")
	} else if policyType == "seccomp" {
		parent = filepath.Join(scPolicyDir, subdir)
		fwParent = filepath.Join(scFrameworkPolicyDir, "policygroups")
	} else {
		return "", errPolicyTypeNotFound
	}

	// Nothing to find if caps is empty
	if len(caps) == 0 {
		return "", nil
	}

	found := false
	badCap := ""
	var p bytes.Buffer
	for _, c := range caps {
		// Always prefer system policy
		policyDirs := []string{parent, fwParent}
		for _, dir := range policyDirs {
			fn := filepath.Join(dir, c)
			tmp, err := ioutil.ReadFile(fn)
			if err == nil {
				p.Write(tmp)
				if c != caps[len(caps)-1] {
					p.Write([]byte("\n"))
				}
				found = true
				break
			}
		}
		if found == false {
			badCap = c
			break
		}
	}

	if found == false {
		return "", &errPolicyNotFound{"cap", badCap}
	}

	return p.String(), nil
}

// TODO: once verified, reorganize all these
func getAppArmorVars(appID *securityAppID) string {
	aavars := "\n# Specified profile variables\n"
	aavars += fmt.Sprintf("@{APP_APPNAME}=\"%s\"\n", appID.Appname)
	aavars += fmt.Sprintf("@{APP_ID_DBUS}=\"%s\"\n", dbusPath(appID.AppID))
	aavars += fmt.Sprintf("@{APP_PKGNAME_DBUS}=\"%s\"\n", dbusPath(appID.Pkgname))
	aavars += fmt.Sprintf("@{APP_PKGNAME}=\"%s\"\n", appID.Pkgname)
	aavars += fmt.Sprintf("@{APP_VERSION}=\"%s\"\n", appID.Version)
	aavars += "@{INSTALL_DIR}=\"{/apps,/oem}\"\n"
	aavars += "# Deprecated:\n"
	aavars += "@{CLICK_DIR}=\"{/apps,/oem}\""

	return aavars
}

func genAppArmorPathRule(path string, access string) (string, error) {
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "@{") {
		logger.Noticef("Bad path: %s", path)
		return "", errPolicyGen
	}

	owner := ""
	if strings.HasPrefix(path, "/home") || strings.HasPrefix(path, "@{HOME") {
		owner = "owner "
	}

	rules := ""
	if strings.HasSuffix(path, "/") {
		rules += fmt.Sprintf("%s %s,\n", path, access)
		rules += fmt.Sprintf("%s%s** %s,\n", owner, path, access)
	} else if strings.HasSuffix(path, "/**") || strings.HasSuffix(path, "/*") {
		rules += fmt.Sprintf("%s/ %s,\n", filepath.Dir(path), access)
		rules += fmt.Sprintf("%s%s %s,\n", owner, path, access)
	} else {
		rules += fmt.Sprintf("%s%s %s,\n", owner, path, access)
	}

	return rules, nil
}

func getAppArmorTemplatedPolicy(m *packageYaml, appID *securityAppID, template string, caps []string, overrides *SecurityAppArmorOverrideDefinition) (string, error) {
	t, err := findTemplate(template, "apparmor")
	if err != nil {
		return "", err
	}
	p, err := findCaps(caps, template, "apparmor")
	if err != nil {
		return "", err
	}

	aaPolicy := strings.Replace(t, "\n###VAR###\n", getAppArmorVars(appID)+"\n", 1)
	aaPolicy = strings.Replace(aaPolicy, "\n###PROFILEATTACH###", fmt.Sprintf("\nprofile \"%s\"", appID.AppID), 1)

	aacaps := ""
	if p == "" {
		aacaps += "# No caps (policy groups) specified\n"
	} else {
		aacaps += "# Rules specified via caps (policy groups)\n"
		prefix := findWhitespacePrefix(t, "###POLICYGROUPS###")
		for _, line := range strings.Split(p, "\n") {
			if len(line) == 0 {
				aacaps += line + "\n"
			} else {
				aacaps += fmt.Sprintf("%s%s\n", prefix, line)
			}
		}
	}
	aaPolicy = strings.Replace(aaPolicy, "###POLICYGROUPS###\n", aacaps, 1)

	if overrides == nil || overrides.ReadPaths == nil {
		aaPolicy = strings.Replace(aaPolicy, "###READS###\n", "# No read paths specified\n", 1)
	} else {
		s := "# Additional read-paths from security-override\n"
		prefix := findWhitespacePrefix(t, "###READS###")
		for _, readpath := range overrides.ReadPaths {
			rules, err := genAppArmorPathRule(strings.Trim(readpath, " "), "rk")
			if err != nil {
				return "", err
			}
			lines := strings.Split(rules, "\n")
			for _, rule := range lines {
				s += fmt.Sprintf("%s%s\n", prefix, rule)
			}
		}
		aaPolicy = strings.Replace(aaPolicy, "###READS###\n", s, 1)
	}

	if overrides == nil || overrides.WritePaths == nil {
		aaPolicy = strings.Replace(aaPolicy, "###WRITES###\n", "# No write paths specified\n", 1)
	} else {
		s := "# Additional write-paths from security-override\n"
		prefix := findWhitespacePrefix(t, "###WRITES###")
		for _, writepath := range overrides.WritePaths {
			rules, err := genAppArmorPathRule(strings.Trim(writepath, " "), "rwk")
			if err != nil {
				return "", err
			}
			lines := strings.Split(rules, "\n")
			for _, rule := range lines {
				s += fmt.Sprintf("%s%s\n", prefix, rule)
			}
		}
		aaPolicy = strings.Replace(aaPolicy, "###WRITES###\n", s, 1)
	}

	if overrides == nil || overrides.Abstractions == nil {
		aaPolicy = strings.Replace(aaPolicy, "###ABSTRACTIONS###\n", "# No abstractions specified\n", 1)
	} else {
		s := "# Additional abstractions from security-override\n"
		prefix := findWhitespacePrefix(t, "###ABSTRACTIONS###")
		for _, abs := range overrides.Abstractions {
			s += fmt.Sprintf("%s#include <abstractions/%s>\n", prefix, abs)
		}
		aaPolicy = strings.Replace(aaPolicy, "###ABSTRACTIONS###\n", s, 1)
	}

	return aaPolicy, nil
}

func getSeccompTemplatedPolicy(m *packageYaml, appID *securityAppID, template string, caps []string, overrides *SecuritySeccompOverrideDefinition) (string, error) {
	t, err := findTemplate(template, "seccomp")
	if err != nil {
		return "", err
	}
	p, err := findCaps(caps, template, "seccomp")
	if err != nil {
		return "", err
	}

	scPolicy := t + "\n" + p

	if overrides != nil && overrides.Syscalls != nil {
		scPolicy += "\n# Addtional syscalls from security-override\n"
		for _, syscall := range overrides.Syscalls {
			scPolicy += fmt.Sprintf("%s\n", syscall)
		}
	}

	scPolicy = strings.Replace(scPolicy, "\ndeny ", "\n# EXPLICITLY DENIED: ", -1)

	return scPolicy, nil
}

func getAppArmorCustomPolicy(m *packageYaml, appID *securityAppID, fn string) (string, error) {
	custom, err := ioutil.ReadFile(fn)
	if err != nil {
		return "", err
	}

	aaPolicy := strings.Replace(string(custom), "\n###VAR###\n", getAppArmorVars(appID)+"\n", 1)
	aaPolicy = strings.Replace(aaPolicy, "\n###PROFILEATTACH###", fmt.Sprintf("\nprofile \"%s\"", appID.AppID), 1)

	return aaPolicy, nil
}

func getSeccompCustomPolicy(m *packageYaml, appID *securityAppID, fn string) (string, error) {
	var custom bytes.Buffer
	tmp, err := ioutil.ReadFile(fn)
	if err != nil {
		return "", err
	}
	custom.Write(tmp)
	return custom.String(), nil
}

func getAppID(appID string) (*securityAppID, error) {
	tmp := strings.Split(appID, "_")
	if len(tmp) != 3 {
		return nil, errInvalidAppID
	}
	id := securityAppID{
		AppID:   appID,
		Pkgname: tmp[0],
		Appname: tmp[1],
		Version: tmp[2],
	}
	return &id, nil
}

func loadAppArmorPolicy(fn string) ([]byte, error) {
	args := []string{
		"/sbin/apparmor_parser",
		"-r",
		"--write-cache",
		"-L", aaCacheDir,
		fn,
	}
	content, err := runAppArmorParser(args...)
	if err != nil {
		logger.Noticef("%v failed", args)
	}
	return content, err
}

func (m *packageYaml) removeOneSecurityPolicy(name, baseDir string) error {
	profileName, err := getSecurityProfile(m, filepath.Base(name), baseDir)
	if err != nil {
		return err
	}

	// seccomp profile
	fn := filepath.Join(dirs.SnapSeccompDir, profileName)
	if err := os.Remove(fn); err != nil && !os.IsNotExist(err) {
		return err
	}

	// apparmor cache
	fn = filepath.Join(aaCacheDir, profileName)
	if err := os.Remove(fn); err != nil && !os.IsNotExist(err) {
		return err
	}

	// apparmor profile
	fn = filepath.Join(dirs.SnapAppArmorDir, profileName)
	if err := os.Remove(fn); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

func removePolicy(m *packageYaml, baseDir string) error {
	for _, service := range m.ServiceYamls {
		if err := m.removeOneSecurityPolicy(service.Name, baseDir); err != nil {
			return err
		}
	}

	for _, binary := range m.Binaries {
		if err := m.removeOneSecurityPolicy(binary.Name, baseDir); err != nil {
			return err
		}
	}

	return nil
}

func (sd *SecurityDefinitions) generatePolicyForServiceBinary(m *packageYaml, name string, baseDir string) error {
	appID, err := getSecurityProfile(m, name, baseDir)
	if err != nil {
		logger.Noticef("Failed to obtain APP_ID for %s: %v", name, err)
		return err
	}

	id, err := getAppID(appID)
	if err != nil {
		logger.Noticef("Failed to obtain APP_ID for %s: %v", name, err)
		return err
	}

	aaPolicy := ""
	scPolicy := ""
	if sd.SecurityPolicy != nil {
		aaPolicy, err = getAppArmorCustomPolicy(m, id, filepath.Join(baseDir, sd.SecurityPolicy.AppArmor))
		if err != nil {
			logger.Noticef("Failed to generate custom AppArmor policy for %s: %v", name, err)
			return err
		}
		scPolicy, err = getSeccompCustomPolicy(m, id, filepath.Join(baseDir, sd.SecurityPolicy.Seccomp))
		if err != nil {
			logger.Noticef("Failed to generate custom seccomp policy for %s: %v", name, err)
			return err
		}
	} else {
		if sd.SecurityOverride == nil || sd.SecurityOverride.AppArmor == nil {
			aaPolicy, err = getAppArmorTemplatedPolicy(m, id, sd.SecurityTemplate, sd.SecurityCaps, nil)
		} else {
			aaPolicy, err = getAppArmorTemplatedPolicy(m, id, sd.SecurityTemplate, sd.SecurityCaps, sd.SecurityOverride.AppArmor)
		}
		if err != nil {
			logger.Noticef("Failed to generate AppArmor policy for %s: %v", name, err)
			return err
		}

		if sd.SecurityOverride == nil || sd.SecurityOverride.Seccomp == nil {
			scPolicy, err = getSeccompTemplatedPolicy(m, id, sd.SecurityTemplate, sd.SecurityCaps, nil)
		} else {
			scPolicy, err = getSeccompTemplatedPolicy(m, id, sd.SecurityTemplate, sd.SecurityCaps, sd.SecurityOverride.Seccomp)
		}
		if err != nil {
			logger.Noticef("Failed to generate seccomp policy for %s: %v", name, err)
			return err
		}
	}

	scFn := filepath.Join(dirs.SnapSeccompDir, id.AppID)
	os.MkdirAll(filepath.Dir(scFn), 0755)
	err = ioutil.WriteFile(scFn, []byte(scPolicy), 0644)
	if err != nil {
		logger.Noticef("Failed to write seccomp policy for %s: %v", name, err)
		return err
	}

	aaFn := filepath.Join(dirs.SnapAppArmorDir, id.AppID)
	os.MkdirAll(filepath.Dir(aaFn), 0755)
	err = ioutil.WriteFile(aaFn, []byte(aaPolicy), 0644)
	if err != nil {
		logger.Noticef("Failed to write AppArmor policy for %s: %v", name, err)
		return err
	}
	out, err := loadAppArmorPolicy(aaFn)
	if err != nil {
		logger.Noticef("Failed to load AppArmor policy for %s: %v\n:%s", name, err, out)
		return err
	}

	return nil
}

func generatePolicy(m *packageYaml, baseDir string) error {
	var err error
	defaultPolicyVendor, err = findUbuntuFlavor()
	if err != nil {
		return err
	}
	defaultPolicyVersion, err = findUbuntuVersion()
	if err != nil {
		return err
	}

	var foundError error

	for _, service := range m.ServiceYamls {
		err := service.generatePolicyForServiceBinary(m, service.Name, baseDir)
		if err != nil {
			foundError = err
			logger.Noticef("Failed to obtain APP_ID for %s: %v", service.Name, err)
			continue
		}
	}

	for _, binary := range m.Binaries {
		err := binary.generatePolicyForServiceBinary(m, binary.Name, baseDir)
		if err != nil {
			foundError = err
			logger.Noticef("Failed to obtain APP_ID for %s: %v", binary.Name, err)
			continue
		}
	}

	if foundError != nil {
		return foundError
	}

	return nil
}

// regeneratePolicyForSnap is used to regenerate all security policy for a
// given snap
func regeneratePolicyForSnap(snapname string) error {
	globExpr := filepath.Join(dirs.SnapAppArmorDir, fmt.Sprintf("%s_*", snapname))
	matches, err := filepath.Glob(globExpr)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		// Nothing to regenerate is not an error
		return nil
	}

	appliedVersion := ""
	for _, profile := range matches {
		appID, err := getAppID(filepath.Base(profile))
		if err != nil {
			return err
		}
		if appID.Version != appliedVersion {
			fn := filepath.Join(dirs.SnapAppsDir, appID.Pkgname, appID.Version, "meta", "package.yaml")
			if !helpers.FileExists(fn) {
				continue
			}
			err := GeneratePolicyFromFile(fn, true)
			if err != nil {
				return err
			}
			appliedVersion = appID.Version
		}
	}

	return nil
}

// GeneratePolicyFromFile is used to generate security policy on the system
// from the specified manifest file name
func GeneratePolicyFromFile(fn string, force bool) error {
	m, err := parsePackageYamlFile(fn)
	if err != nil {
		return err
	}

	if m.Type == "" || m.Type == pkg.TypeApp {
		_, err = originFromYamlPath(fn)
		if err != nil {
			if err == ErrInvalidPart {
				err = errOriginNotFound
			}
			return err
		}
	}

	// TODO: verify cache files here

	baseDir := filepath.Dir(filepath.Dir(fn))
	err = generatePolicy(m, baseDir)
	if err != nil {
		return err
	}

	return err
}
