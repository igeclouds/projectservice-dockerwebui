package hcsshim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	winio "github.com/Microsoft/go-winio/vhd"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

const (
	// DefaultLCOWScratchSizeGB is the size of the default LCOW sandbox & scratch in GB
	DefaultLCOWScratchSizeGB = 20

	// defaultLCOWVhdxBlockSizeMB is the block-size for the sandbox/scratch VHDx's this package can create.
	defaultLCOWVhdxBlockSizeMB = 1
)

// CreateLCOWScratch uses a utility VM to create an empty scratch disk of a requested size.
// It has a caching capability. If the cacheFile exists, and the request is for a default
// size, a copy of that is made to the target. If the size is non-default, or the cache file
// does not exist, it uses a utility VM to create target. It is the responsibility of the
// caller to synchronise simultaneous attempts to create the cache file.

func CreateLCOWScratch(uvm Container, destFile string, sizeGB uint32, cacheFile string) error {
	// Smallest we can accept is the default sandbox size as we can't size down, only expand.
	if sizeGB < DefaultLCOWScratchSizeGB {
		sizeGB = DefaultLCOWScratchSizeGB
	}

	logrus.Debugf("hcsshim::CreateLCOWScratch: Dest:%s size:%dGB cache:%s", destFile, sizeGB, cacheFile)

	// Retrieve from cache if the default size and already on disk
	if cacheFile != "" && sizeGB == DefaultLCOWScratchSizeGB {
		if _, err := os.Stat(cacheFile); err == nil {
			if err := CopyFile(cacheFile, destFile, false); err != nil {
				return fmt.Errorf("failed to copy cached file '%s' to '%s': %s", cacheFile, destFile, err)
			}
			logrus.Debugf("hcsshim::CreateLCOWScratch: %s fulfilled from cache", destFile)
			return nil
		}
	}

	if uvm == nil {
		return fmt.Errorf("cannot create scratch disk as cache is not present and no utility VM supplied")
	}
	uvmc := uvm.(*container)

	// Create the VHDX
	if err := winio.CreateVhdx(destFile, sizeGB, defaultLCOWVhdxBlockSizeMB); err != nil {
		return fmt.Errorf("failed to create VHDx %s: %s", destFile, err)
	}

	uvmc.DebugLCOWGCS()

	controller, lun, err := AddSCSIDisk(uvm, destFile, "")
	if err != nil {
		// TODO Rollback
	}

	logrus.Debugf("hcsshim::CreateLCOWScratch: %s at C=%d L=%d", destFile, controller, lun)

	// Validate /sys/bus/scsi/devices/C:0:0:L exists as a directory
	testdCommand := []string{"test", "-d", fmt.Sprintf("/sys/bus/scsi/devices/%d:0:0:%d", controller, lun)}
	testdProc, _, err := uvmc.CreateProcessEx(&CreateProcessEx{
		OCISpecification: &specs.Spec{
			Process: &specs.Process{Args: testdCommand},
			Linux:   &specs.Linux{},
		},
		CreateInUtilityVm: true,
	})
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to run %+v following hot-add %s to utility VM: %s", testdCommand, destFile, err)
	}
	defer testdProc.Close()

	testdProc.WaitTimeout(defaultTimeoutSeconds)
	testdExitCode, err := testdProc.ExitCode()
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from from %+v following hot-add %s to utility VM: %s", testdCommand, destFile, err)
	}
	if testdExitCode != 0 {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("`%+v` return non-zero exit code (%d) following hot-add %s to utility VM", testdCommand, testdExitCode, destFile)
	}

	// Get the device from under the block subdirectory by doing a simple ls. This will come back as (eg) `sda`
	var lsOutput bytes.Buffer
	lsCommand := []string{"ls", fmt.Sprintf("/sys/bus/scsi/devices/%d:0:0:%d/block", controller, lun)}
	lsProc, _, err := uvmc.CreateProcessEx(&CreateProcessEx{
		OCISpecification: &specs.Spec{
			Process: &specs.Process{Args: lsCommand},
			Linux:   &specs.Linux{},
		},
		CreateInUtilityVm: true,
		Stdout:            &lsOutput,
	})
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", lsCommand, destFile, err)
	}
	defer lsProc.Close()
	lsProc.WaitTimeout(defaultTimeoutSeconds)
	lsExitCode, err := lsProc.ExitCode()
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from `%+v` following hot-add %s to utility VM: %s", lsCommand, destFile, err)
	}
	if lsExitCode != 0 {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("`%+v` return non-zero exit code (%d) following hot-add %s to utility VM", lsCommand, lsExitCode, destFile)
	}
	device := fmt.Sprintf(`/dev/%s`, strings.TrimSpace(lsOutput.String()))
	logrus.Debugf("hcsshim: CreateExt4Vhdx: %s: device at %s", destFile, device)

	// Format it ext4
	mkfsCommand := []string{"mkfs.ext4", "-q", "-E", "lazy_itable_init=1", "-O", `^has_journal,sparse_super2,uninit_bg,^resize_inode`, device}
	var mkfsStderr bytes.Buffer
	mkfsProc, _, err := uvmc.CreateProcessEx(&CreateProcessEx{
		OCISpecification: &specs.Spec{
			Process: &specs.Process{Args: mkfsCommand},
			Linux:   &specs.Linux{},
		},
		CreateInUtilityVm: true,
		Stderr:            &mkfsStderr,
	})
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", mkfsCommand, destFile, err)
	}
	defer mkfsProc.Close()
	mkfsProc.WaitTimeout(defaultTimeoutSeconds)
	mkfsExitCode, err := mkfsProc.ExitCode()
	if err != nil {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from `%+v` following hot-add %s to utility VM: %s", mkfsCommand, destFile, err)
	}
	if mkfsExitCode != 0 {
		removeSCSIDisk(uvm, destFile, controller, lun)
		return fmt.Errorf("`%+v` return non-zero exit code (%d) following hot-add %s to utility VM: %s", mkfsCommand, mkfsExitCode, destFile, strings.TrimSpace(mkfsStderr.String()))
	}

	// Hot-Remove before we copy it
	if err := removeSCSIDisk(uvm, destFile, controller, lun); err != nil {
		return fmt.Errorf("failed to hot-remove: %s", err)
	}

	// Populate the cache.
	if cacheFile != "" && (sizeGB == DefaultLCOWScratchSizeGB) {
		if err := CopyFile(destFile, cacheFile, true); err != nil {
			return fmt.Errorf("failed to seed cache '%s' from '%s': %s", destFile, cacheFile, err)
		}
	}

	logrus.Debugf("hcsshim::CreateLCOWScratch: %s created (non-cache)", destFile)
	return nil
}

// TarToVhd streams a tarstream contained in an io.Reader to a fixed vhd file
func TarToVhd(uvm Container, targetVHDFile string, reader io.Reader) (int64, error) {
	logrus.Debugf("hcsshim: TarToVhd: %s", targetVHDFile)

	if uvm == nil {
		return 0, fmt.Errorf("cannot Tar2Vhd as no utility VM supplied")
	}
	defer uvm.DebugLCOWGCS()

	outFile, err := os.Create(targetVHDFile)
	if err != nil {
		return 0, fmt.Errorf("tar2vhd failed to create %s: %s", targetVHDFile, err)
	}
	defer outFile.Close()
	// BUGBUG Delete the file on failure

	tar2vhd, byteCounts, err := uvm.CreateProcessEx(&CreateProcessEx{
		OCISpecification: &specs.Spec{
			Process: &specs.Process{Args: []string{"tar2vhd"}},
			Linux:   &specs.Linux{},
		},
		CreateInUtilityVm: true,
		Stdin:             reader,
		Stdout:            outFile,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to start tar2vhd for %s: %s", targetVHDFile, err)
	}
	defer tar2vhd.Close()

	logrus.Debugf("hcsshim: TarToVhd: %s created, %d bytes", targetVHDFile, byteCounts.Out)
	return byteCounts.Out, err
}

//// VhdToTar does what is says - it exports a VHD in a specified
//// folder (either a read-only layer.vhd, or a read-write sandbox.vhd) to a
//// ReadCloser containing a tar-stream of the layers contents.
//func VhdToTar(uvm Container, vhdFile string, uvmMountPath string, isSandbox bool, vhdSize int64) (io.ReadCloser, error) {
//	logrus.Debugf("hcsshim: VhdToTar: %s isSandbox: %t", vhdFile, isSandbox)

//	if config.Uvm == nil {
//		return nil, fmt.Errorf("cannot VhdToTar as no utility VM is in configuration")
//	}

//	defer uvm.DebugLCOWGCS()

//	vhdHandle, err := os.Open(vhdFile)
//	if err != nil {
//		return nil, fmt.Errorf("hcsshim: VhdToTar: failed to open %s: %s", vhdFile, err)
//	}
//	defer vhdHandle.Close()
//	logrus.Debugf("hcsshim: VhdToTar: exporting %s, size %d, isSandbox %t", vhdHandle.Name(), vhdSize, isSandbox)

//	// Different binary depending on whether a RO layer or a RW sandbox
//	command := "vhd2tar"
//	if isSandbox {
//		command = fmt.Sprintf("exportSandbox -path %s", uvmMountPath)
//	}

//	// Start the binary in the utility VM
//	proc, stdin, stdout, _, err := config.createLCOWUVMProcess(command)
//	if err != nil {
//		return nil, fmt.Errorf("hcsshim: VhdToTar: %s: failed to create utils process %s: %s", vhdHandle.Name(), command, err)
//	}

//	if !isSandbox {
//		// Send the VHD contents to the utility VM processes stdin handle if not a sandbox
//		logrus.Debugf("hcsshim: VhdToTar: copying the layer VHD into the utility VM")
//		if _, err = copyWithTimeout(stdin, vhdHandle, vhdSize, processOperationTimeoutSeconds, fmt.Sprintf("vhdtotarstream: sending %s to %s", vhdHandle.Name(), command)); err != nil {
//			proc.Close()
//			return nil, fmt.Errorf("hcsshim: VhdToTar: %s: failed to copyWithTimeout on the stdin pipe (to utility VM): %s", vhdHandle.Name(), err)
//		}
//	}

//	// Start a goroutine which copies the stdout (ie the tar stream)
//	reader, writer := io.Pipe()
//	go func() {
//		defer writer.Close()
//		defer proc.Close()
//		logrus.Debugf("hcsshim: VhdToTar: copying tar stream back from the utility VM")
//		bytes, err := copyWithTimeout(writer, stdout, vhdSize, processOperationTimeoutSeconds, fmt.Sprintf("vhdtotarstream: copy tarstream from %s", command))
//		if err != nil {
//			logrus.Errorf("hcsshim: VhdToTar: %s:  copyWithTimeout on the stdout pipe (from utility VM) failed: %s", vhdHandle.Name(), err)
//		}
//		logrus.Debugf("hcsshim: VhdToTar: copied %d bytes of the tarstream of %s from the utility VM", bytes, vhdHandle.Name())
//	}()

//	// Return the read-side of the pipe connected to the goroutine which is reading from the stdout of the process in the utility VM
//	return reader, nil
//}