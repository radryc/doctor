package buildinfo

import (
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
)

var (
	Version   = ""
	Commit    = ""
	BuildTime = ""
)

type Info struct {
	Version   string
	Revision  string
	Branch    string
	BuildDate string
	GoVersion string
	Modified  bool
}

func Current() Info {
	info := Info{
		Version:   strings.TrimSpace(Version),
		Revision:  strings.TrimSpace(Commit),
		BuildDate: strings.TrimSpace(BuildTime),
		GoVersion: runtime.Version(),
	}

	if runtimeInfo, ok := runtimedebug.ReadBuildInfo(); ok {
		if strings.TrimSpace(runtimeInfo.GoVersion) != "" {
			info.GoVersion = strings.TrimSpace(runtimeInfo.GoVersion)
		}
		for _, setting := range runtimeInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Revision == "" {
					info.Revision = strings.TrimSpace(setting.Value)
				}
			case "vcs.branch":
				if info.Branch == "" {
					info.Branch = strings.TrimSpace(setting.Value)
				}
			case "vcs.time":
				if info.BuildDate == "" {
					info.BuildDate = strings.TrimSpace(setting.Value)
				}
			case "vcs.modified":
				info.Modified = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
			}
		}
	}

	if info.Version == "" {
		info.Version = fallbackVersion(info.Revision, info.Modified)
	}
	if info.Revision == "" {
		info.Revision = "unknown"
	}

	return info
}

func (i Info) StatusFields() map[string]string {
	return map[string]string{
		"version":   strings.TrimSpace(i.Version),
		"revision":  strings.TrimSpace(i.Revision),
		"branch":    strings.TrimSpace(i.Branch),
		"buildUser": "",
		"buildDate": strings.TrimSpace(i.BuildDate),
		"goVersion": strings.TrimSpace(i.GoVersion),
	}
}

func fallbackVersion(revision string, modified bool) string {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}
