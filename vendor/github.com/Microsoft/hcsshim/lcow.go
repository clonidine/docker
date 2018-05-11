package hcsshim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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

func getLCOWSettings(createOptions *CreateOptions) {
	createOptions.lcowkird = valueFromStringMap(createOptions.Options, HCSOPTION_LCOW_KIRD_PATH)
	if createOptions.lcowkird == "" {
		createOptions.lcowkird = filepath.Join(os.Getenv("ProgramFiles"), "Linux Containers")
	}
	createOptions.lcowkernel = valueFromStringMap(createOptions.Options, HCSOPTION_LCOW_KERNEL_FILE)
	if createOptions.lcowkernel == "" {
		createOptions.lcowkernel = "bootx64.efi"
	}
	createOptions.lcowinitrd = valueFromStringMap(createOptions.Options, HCSOPTION_LCOW_INITRD_FILE)
	if createOptions.lcowinitrd == "" {
		createOptions.lcowinitrd = "initrd.img"
	}
	createOptions.lcowbootparams = valueFromStringMap(createOptions.Options, HCSOPTION_LCOW_BOOT_PARAMETERS)
}

// createLCOWv1 creates a Linux (LCOW) container using the V1 schema.
func createLCOWv1(createOptions *CreateOptions) (Container, error) {

	configuration := &ContainerConfig{
		HvPartition:   true,
		Name:          createOptions.id,
		SystemType:    "container",
		ContainerType: "linux",
		Owner:         createOptions.owner,
		TerminateOnLastHandleClosed: true,
	}
	configuration.HvRuntime = &HvRuntime{
		ImagePath:           createOptions.lcowkird,
		LinuxKernelFile:     createOptions.lcowkernel,
		LinuxInitrdFile:     createOptions.lcowinitrd,
		LinuxBootParameters: createOptions.lcowbootparams,
	}

	// TODO These checks were elsewhere. In common with v2 too.
	//	if _, err := os.Stat(filepath.Join(config.KirdPath, config.KernelFile)); os.IsNotExist(err) {
	//		return fmt.Errorf("kernel '%s' not found", filepath.Join(config.KirdPath, config.KernelFile))
	//	}
	//	if _, err := os.Stat(filepath.Join(config.KirdPath, config.InitrdFile)); os.IsNotExist(err) {
	//		return fmt.Errorf("initrd '%s' not found", filepath.Join(config.KirdPath, config.InitrdFile))
	//	}

	//	// Ensure all the MappedVirtualDisks exist on the host
	//	for _, mvd := range config.MappedVirtualDisks {
	//		if _, err := os.Stat(mvd.HostPath); err != nil {
	//			return fmt.Errorf("mapped virtual disk '%s' not found", mvd.HostPath)
	//		}
	//		if mvd.ContainerPath == "" {
	//			return fmt.Errorf("mapped virtual disk '%s' requested without a container path", mvd.HostPath)
	//		}
	//	}

	if createOptions.Spec.Windows != nil {
		// Strip off the top-most layer as that's passed in separately to HCS
		if len(createOptions.Spec.Windows.LayerFolders) > 0 {
			configuration.LayerFolderPath = createOptions.Spec.Windows.LayerFolders[len(createOptions.Spec.Windows.LayerFolders)-1]
			layerFolders := createOptions.Spec.Windows.LayerFolders[:len(createOptions.Spec.Windows.LayerFolders)-1]

			for _, layerPath := range layerFolders {
				_, filename := filepath.Split(layerPath)
				g, err := NameToGuid(filename)
				if err != nil {
					return nil, err
				}
				configuration.Layers = append(configuration.Layers, Layer{
					ID:   g.ToString(),
					Path: filepath.Join(layerPath, "layer.vhd"),
				})
			}
		}

		if createOptions.Spec.Windows.Network != nil {
			configuration.EndpointList = createOptions.Spec.Windows.Network.EndpointList
			configuration.AllowUnqualifiedDNSQuery = createOptions.Spec.Windows.Network.AllowUnqualifiedDNSQuery
			if createOptions.Spec.Windows.Network.DNSSearchList != nil {
				configuration.DNSSearchList = strings.Join(createOptions.Spec.Windows.Network.DNSSearchList, ",")
			}
			configuration.NetworkSharedContainerName = createOptions.Spec.Windows.Network.NetworkSharedContainerName
		}
	}

	// Add the mounts (volumes, bind mounts etc) to the structure. We have to do
	// some translation for both the mapped directories passed into HCS and in
	// the spec.
	//
	// For HCS, we only pass in the mounts from the spec which are type "bind".
	// Further, the "ContainerPath" field (which is a little mis-leadingly
	// named when it applies to the utility VM rather than the container in the
	// utility VM) is moved to under /tmp/gcs/<ID>/binds, where this is passed
	// by the caller through a 'uvmpath' option.
	//
	// We do similar translation for the mounts in the spec by stripping out
	// the uvmpath option, and translating the Source path to the location in the
	// utility VM calculated above.
	//
	// From inside the utility VM, you would see a 9p mount such as in the following
	// where a host folder has been mapped to /target. The line with /tmp/gcs/<ID>/binds
	// specifically:
	//
	//	/ # mount
	//	rootfs on / type rootfs (rw,size=463736k,nr_inodes=115934)
	//	proc on /proc type proc (rw,relatime)
	//	sysfs on /sys type sysfs (rw,relatime)
	//	udev on /dev type devtmpfs (rw,relatime,size=498100k,nr_inodes=124525,mode=755)
	//	tmpfs on /run type tmpfs (rw,relatime)
	//	cgroup on /sys/fs/cgroup type cgroup (rw,relatime,cpuset,cpu,cpuacct,blkio,memory,devices,freezer,net_cls,perf_event,net_prio,hugetlb,pids,rdma)
	//	mqueue on /dev/mqueue type mqueue (rw,relatime)
	//	devpts on /dev/pts type devpts (rw,relatime,mode=600,ptmxmode=000)
	//	/binds/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/target on /binds/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/target type 9p (rw,sync,dirsync,relatime,trans=fd,rfdno=6,wfdno=6)
	//	/dev/pmem0 on /tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/layer0 type ext4 (ro,relatime,block_validity,delalloc,norecovery,barrier,dax,user_xattr,acl)
	//	/dev/sda on /tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/scratch type ext4 (rw,relatime,block_validity,delalloc,barrier,user_xattr,acl)
	//	overlay on /tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/rootfs type overlay (rw,relatime,lowerdir=/tmp/base/:/tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/layer0,upperdir=/tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/scratch/upper,workdir=/tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc/scratch/work)
	//
	//  /tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc # ls -l
	//	total 16
	//	drwx------    3 0        0               60 Sep  7 18:54 binds
	//	-rw-r--r--    1 0        0             3345 Sep  7 18:54 config.json
	//	drwxr-xr-x   10 0        0             4096 Sep  6 17:26 layer0
	//	drwxr-xr-x    1 0        0             4096 Sep  7 18:54 rootfs
	//	drwxr-xr-x    5 0        0             4096 Sep  7 18:54 scratch
	//
	//	/tmp/gcs/b3ea9126d67702173647ece2744f7c11181c0150e9890fc9a431849838033edc # ls -l binds
	//	total 0
	//	drwxrwxrwt    2 0        0             4096 Sep  7 16:51 target

	mds := []MappedDir{}
	specMounts := []specs.Mount{}
	for _, mount := range createOptions.Spec.Mounts {
		specMount := mount
		if mount.Type == "bind" {
			// Strip out the uvmpath from the options
			updatedOptions := []string{}
			uvmPath := ""
			readonly := false
			for _, opt := range mount.Options {
				dropOption := false
				elements := strings.SplitN(opt, "=", 2)
				switch elements[0] {
				case "uvmpath":
					uvmPath = elements[1]
					dropOption = true
				case "rw":
				case "ro":
					readonly = true
				case "rbind":
				default:
					return nil, fmt.Errorf("unsupported option %q", opt)
				}
				if !dropOption {
					updatedOptions = append(updatedOptions, opt)
				}
			}
			mount.Options = updatedOptions
			if uvmPath == "" {
				return nil, fmt.Errorf("no uvmpath for bind mount %+v", mount)
			}
			md := MappedDir{
				HostPath:          mount.Source,
				ContainerPath:     path.Join(uvmPath, mount.Destination),
				CreateInUtilityVM: true,
				ReadOnly:          readonly,
			}
			mds = append(mds, md)
			specMount.Source = path.Join(uvmPath, mount.Destination)
		}
		specMounts = append(specMounts, specMount)
	}
	configuration.MappedDirectories = mds

	container, err := CreateContainer(createOptions.id, configuration)
	if err != nil {
		return nil, err
	}

	// TODO - Not sure why after CreateContainer, but that's how I coded it in libcontainerd and it worked....
	createOptions.Spec.Mounts = specMounts

	logrus.Debugf("createLCOWv1() completed successfully")
	return container, nil
}

func debugCommand(s string) string {
	return fmt.Sprintf(`echo -e 'DEBUG COMMAND: %s\\n--------------\\n';%s;echo -e '\\n\\n';`, s, s)
}

// DebugLCOWGCS extracts logs from the GCS in LCOW. It's a useful hack for debugging,
// but not necessarily optimal, but all that is available to us in RS3.
func (container *container) DebugLCOWGCS() {
	if logrus.GetLevel() < logrus.DebugLevel || len(os.Getenv("HCSSHIM_LCOW_DEBUG_ENABLE")) == 0 {
		return
	}

	var out bytes.Buffer
	cmd := os.Getenv("HCSSHIM_LCOW_DEBUG_COMMAND")
	if cmd == "" {
		cmd = `sh -c "`
		cmd += debugCommand("kill -10 `pidof gcs`") // SIGUSR1 for stackdump
		cmd += debugCommand("ls -l /tmp")
		cmd += debugCommand("cat /tmp/gcs.log")
		cmd += debugCommand("cat /tmp/gcs/gcs-stacks*")
		cmd += debugCommand("cat /tmp/gcs/paniclog*")
		cmd += debugCommand("ls -l /tmp/gcs")
		cmd += debugCommand("ls -l /tmp/gcs/*")
		cmd += debugCommand("cat /tmp/gcs/*/config.json")
		cmd += debugCommand("ls -lR /var/run/gcsrunc")
		cmd += debugCommand("cat /tmp/gcs/global-runc.log")
		cmd += debugCommand("cat /tmp/gcs/*/runc.log")
		cmd += debugCommand("ps -ef")
		cmd += `"`
	}

	proc, _, err := container.CreateProcessEx(
		&CreateProcessEx{
			OCISpecification: &specs.Spec{
				Process: &specs.Process{Args: []string{cmd}},
				Linux:   &specs.Linux{},
			},
			CreateInUtilityVm: true,
			Stdout:            &out,
		})
	defer func() {
		if proc != nil {
			proc.Kill()
			proc.Close()
		}
	}()
	if err != nil {
		logrus.Debugln("benign failure getting gcs logs: ", err)
	}
	if proc != nil {
		proc.WaitTimeout(time.Duration(int(time.Second) * 30))
	}
	logrus.Debugf("GCS Debugging:\n%s\n\nEnd GCS Debugging", strings.TrimSpace(out.String()))
}

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

	controller, lun, err := AddSCSI(uvm, destFile, "")
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
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to run %+v following hot-add %s to utility VM: %s", testdCommand, destFile, err)
	}
	defer testdProc.Close()

	testdProc.WaitTimeout(defaultTimeoutSeconds)
	testdExitCode, err := testdProc.ExitCode()
	if err != nil {
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from from %+v following hot-add %s to utility VM: %s", testdCommand, destFile, err)
	}
	if testdExitCode != 0 {
		removeSCSI(uvm, destFile, controller, lun)
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
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", lsCommand, destFile, err)
	}
	defer lsProc.Close()
	lsProc.WaitTimeout(defaultTimeoutSeconds)
	lsExitCode, err := lsProc.ExitCode()
	if err != nil {
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from `%+v` following hot-add %s to utility VM: %s", lsCommand, destFile, err)
	}
	if lsExitCode != 0 {
		removeSCSI(uvm, destFile, controller, lun)
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
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to `%+v` following hot-add %s to utility VM: %s", mkfsCommand, destFile, err)
	}
	defer mkfsProc.Close()
	mkfsProc.WaitTimeout(defaultTimeoutSeconds)
	mkfsExitCode, err := mkfsProc.ExitCode()
	if err != nil {
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("failed to get exit code from `%+v` following hot-add %s to utility VM: %s", mkfsCommand, destFile, err)
	}
	if mkfsExitCode != 0 {
		removeSCSI(uvm, destFile, controller, lun)
		return fmt.Errorf("`%+v` return non-zero exit code (%d) following hot-add %s to utility VM: %s", mkfsCommand, mkfsExitCode, destFile, strings.TrimSpace(mkfsStderr.String()))
	}

	// Hot-Remove before we copy it
	if err := removeSCSI(uvm, destFile, controller, lun); err != nil {
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