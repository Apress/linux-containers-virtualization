package specconv // import "github.com/docker/docker/rootless/specconv"

import (
	"io/ioutil"
	"strconv"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// ToRootless converts spec to be compatible with "rootless" runc.
// * Remove non-supported cgroups
// * Fix up OOMScoreAdj
//
// v2Controllers should be non-nil only if running with v2 and systemd.
func ToRootless(spec *specs.Spec, v2Controllers []string) error {
	return toRootless(spec, v2Controllers, getCurrentOOMScoreAdj())
}

func getCurrentOOMScoreAdj() int {
	b, err := ioutil.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		return 0
	}
	i, err := strconv.Atoi(string(b))
	if err != nil {
		return 0
	}
	return i
}

func toRootless(spec *specs.Spec, v2Controllers []string, currentOOMScoreAdj int) error {
	if len(v2Controllers) == 0 {
		// Remove cgroup settings.
		spec.Linux.Resources = nil
		spec.Linux.CgroupsPath = ""
	} else {
		if spec.Linux.Resources != nil {
			m := make(map[string]struct{})
			for _, s := range v2Controllers {
				m[s] = struct{}{}
			}
			// Remove devices: https://github.com/containers/crun/issues/255
			spec.Linux.Resources.Devices = nil
			if _, ok := m["memory"]; !ok {
				spec.Linux.Resources.Memory = nil
			}
			if _, ok := m["cpu"]; !ok {
				spec.Linux.Resources.CPU = nil
			}
			if _, ok := m["cpuset"]; !ok {
				if spec.Linux.Resources.CPU != nil {
					spec.Linux.Resources.CPU.Cpus = ""
					spec.Linux.Resources.CPU.Mems = ""
				}
			}
			if _, ok := m["pids"]; !ok {
				spec.Linux.Resources.Pids = nil
			}
			if _, ok := m["io"]; !ok {
				spec.Linux.Resources.BlockIO = nil
			}
			if _, ok := m["rdma"]; !ok {
				spec.Linux.Resources.Rdma = nil
			}
			spec.Linux.Resources.HugepageLimits = nil
			spec.Linux.Resources.Network = nil
		}
	}

	if spec.Process.OOMScoreAdj != nil && *spec.Process.OOMScoreAdj < currentOOMScoreAdj {
		*spec.Process.OOMScoreAdj = currentOOMScoreAdj
	}
	return nil
}
