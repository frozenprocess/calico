// Copyright (c) 2020-2022 Tigera, Inc. All rights reserved.
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

package xdp

import (
	"fmt"
	"path"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/projectcalico/calico/felix/bpf"
	"github.com/projectcalico/calico/felix/bpf/bpfdefs"
	"github.com/projectcalico/calico/felix/bpf/hook"
	"github.com/projectcalico/calico/felix/bpf/libbpf"
	tcdefs "github.com/projectcalico/calico/felix/bpf/tc/defs"
)

const DetachedID = 0

type AttachPoint struct {
	bpf.AttachPoint
	HookLayoutV4 hook.Layout
	HookLayoutV6 hook.Layout

	Modes []bpf.XDPMode
}

func (ap *AttachPoint) PolicyAllowJumpIdx(family int) int {
	if family == 4 && ap.HookLayoutV4 != nil {
		return ap.HookLayoutV4[hook.SubProgXDPAllowed]
	}
	if family == 6 && ap.HookLayoutV6 != nil {
		return ap.HookLayoutV6[hook.SubProgXDPAllowed]
	}
	return -1
}

func (ap *AttachPoint) PolicyDenyJumpIdx(family int) int {
	if family == 4 && ap.HookLayoutV4 != nil {
		return ap.HookLayoutV4[hook.SubProgXDPDrop]
	}

	if family == 6 && ap.HookLayoutV6 != nil {
		return ap.HookLayoutV6[hook.SubProgXDPDrop]
	}
	return -1
}

func (ap *AttachPoint) Config() string {
	return fmt.Sprintf("%+v", ap)
}

func (ap *AttachPoint) FileName() string {
	logLevel := strings.ToLower(ap.LogLevel)
	if logLevel == "off" {
		logLevel = "no_log"
	}
	return "xdp_" + logLevel + ".o"
}

func (ap *AttachPoint) ProgramName() string {
	return "cali_xdp_preamble"
}

func (ap *AttachPoint) Log() *log.Entry {
	return log.WithFields(log.Fields{
		"iface":    ap.Iface,
		"modes":    ap.Modes,
		"logLevel": ap.LogLevel,
	})
}

func (ap *AttachPoint) AlreadyAttached(object string) bool {
	_, err := ap.ProgramID()
	if err != nil {
		ap.Log().Debugf("Couldn't get the attached XDP program ID. err=%v", err)
		return false
	}
	return true
}

func (ap *AttachPoint) Configuration() *libbpf.XDPGlobalData {
	globalData := &libbpf.XDPGlobalData{}
	if ap.HookLayoutV4 != nil {
		for p, i := range ap.HookLayoutV4 {
			globalData.Jumps[p] = uint32(i)
		}
		globalData.Jumps[tcdefs.ProgIndexPolicy] = uint32(ap.PolicyIdxV4)
	}
	if ap.HookLayoutV6 != nil {
		for p, i := range ap.HookLayoutV6 {
			globalData.JumpsV6[p] = uint32(i)
		}
		globalData.JumpsV6[tcdefs.ProgIndexPolicy] = uint32(ap.PolicyIdxV6)
	}
	in := []byte("---------------")
	copy(in, ap.Iface)
	globalData.IfaceName = string(in)

	return globalData
}

func (ap *AttachPoint) AttachProgram() error {
	// By now the attach type specific generic set of programs is loaded and we
	// only need to load and configure the preamble that will pass the
	// configuration further to the selected set of programs.

	binaryToLoad := path.Join(bpfdefs.ObjectDir, "xdp_preamble.o")
	ap.Log().Infof("Continue with attaching BPF program %s", binaryToLoad)
	obj, err := bpf.LoadObject(binaryToLoad, ap.Configuration())
	if err != nil {
		return fmt.Errorf("error loading %s:%w", binaryToLoad, err)
	}

	oldID, err := ap.ProgramID()
	if err != nil {
		return fmt.Errorf("failed to get the attached XDP program ID: %w", err)
	}

	attachmentSucceeded := false
	for _, mode := range ap.Modes {
		ap.Log().Debugf("Trying to attach XDP program in mode %v - old id: %v", mode, oldID)
		// Force attach the program. If there is already a program attached, the replacement only
		// succeed in the same mode of the current program.
		progID, err := obj.AttachXDP(ap.Iface, ap.ProgramName(), oldID, unix.XDP_FLAGS_REPLACE|uint(mode))
		if err != nil || progID == DetachedID || progID == oldID {
			ap.Log().WithError(err).Warnf("Failed to attach to XDP program %s mode %v", ap.ProgramName(), mode)
		} else {
			ap.Log().Debugf("Successfully attached XDP program in mode %v. ID: %v", mode, progID)
			attachmentSucceeded = true
			break
		}
	}

	if !attachmentSucceeded {
		return fmt.Errorf("failed to attach XDP program with program name %v to interface %v",
			ap.ProgramName(), ap.Iface)
	}

	return nil
}

func (ap *AttachPoint) DetachProgram() error {
	// Get the current XDP program ID, if any.
	progID, err := ap.ProgramID()
	if err != nil {
		return fmt.Errorf("failed to get the attached XDP program ID: %w", err)
	}
	if progID == DetachedID {
		ap.Log().Debugf("No XDP program attached.")
		return nil
	}

	prog, err := bpf.GetProgByID(progID)
	if err != nil {
		return fmt.Errorf("failed to get prog by id %d: %w", progID, err)
	}

	if !strings.HasPrefix(prog.Name, "cali_xdp_preamb") {
		ap.Log().Debugf("Program id %d name %s not ours.", progID, prog.Name)
		return nil
	}

	// Try to remove our XDP program in all modes, until the program ID is 0
	removalSucceeded := false
	for _, mode := range ap.Modes {
		err = libbpf.DetachXDP(ap.Iface, uint(mode))
		ap.Log().Debugf("Trying to detach XDP program in mode %v.", mode)
		if err != nil {
			ap.Log().Debugf("Failed to detach XDP program in mode %v: %v.", mode, err)
			continue
		}
		curProgId, err := ap.ProgramID()
		if err != nil {
			return fmt.Errorf("failed to get the attached XDP program ID: %w", err)
		}

		if curProgId == DetachedID {
			removalSucceeded = true
			ap.Log().Debugf("Successfully detached XDP program.")
			break
		}
	}
	if !removalSucceeded {
		return fmt.Errorf("couldn't remove our XDP program. program ID: %v", progID)
	}

	ap.Log().Infof("XDP program detached. program ID: %v", progID)
	return nil
}

func (ap *AttachPoint) ProgramID() (int, error) {
	progID, err := libbpf.GetXDPProgramID(ap.Iface)
	if err != nil {
		return -1, fmt.Errorf("Couldn't check for XDP program on iface %v: %w", ap.Iface, err)
	}
	return progID, nil
}
