---
sidebar_position: 3
---

# vphone-cli Setup and Usage Guide

Project: https://github.com/Lakr233/vphone-cli

## I. Pre-Setup Checks

### 1. Hardware Requirements

Use the following:

- A physical Mac with Apple silicon.
- macOS 15 or later.
- The full version of Xcode.
- A stable network connection.
- At least 100 GB of available disk space.
- An administrator account and its password.

Do not run this project in VMware, Parallels, UTM, or an Apple Virtual Machine. The project cannot start another Virtualization.framework virtual machine from inside a nested virtual machine.

### 2. Check the Current Environment

Open Terminal and run the following commands one by one:

```bash
uname -m
sw_vers -productVersion
system_profiler SPHardwareDataType | grep "Model Name"
sysctl -n kern.hv_vmm_present
df -h /
```

Expected results:

- `uname -m` should display `arm64`.
- The macOS version must be 15 or later.
- The model must not be `Apple Virtual Machine 1`.
- If `kern.hv_vmm_present` displays `1`, the current macOS system may itself be running inside a virtual machine, and you should not continue.
- There should be sufficient available disk space.

### 3. Install the Full Version of Xcode

Install Xcode from the App Store. After installation, launch Xcode once and accept the license agreement. Then run the following commands in Terminal:

```bash
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -license accept
xcrun -sdk iphoneos --show-sdk-path
```

The last command should output a path to `iPhoneOS.sdk`.

### 4. Install Homebrew

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

Verify the installation:

```bash
brew --version
```

## II. Modify Mac Security Settings: Completely Disable SIP and Configure the AMFI Boot Arguments

### 1. Record the Existing Boot Arguments

In a normal macOS Terminal session, run:

```bash
nvram -p | grep '^boot-args'
```

If the command produces output, take a photo of it or copy and save it. Setting new `boot-args` later will overwrite the existing value.

Then shut down the Mac.

### 2. Enter macOS Recovery Mode

1. After the Mac has completely shut down, press and hold the power button. Do not release it immediately.
2. Continue holding the power button after the screen turns on.
3. Release the power button when you see “Loading startup options” or the gear-shaped “Options” icon.
4. Select **Options**.
5. Click **Continue**.
6. If prompted to select a system volume, select **Macintosh HD**.
7. If prompted to select an administrator user, select the current administrator account and enter its login password.

### 3. Open Terminal in Recovery Mode

After entering the Recovery utilities screen, select **Utilities → Terminal** from the menu bar at the top of the screen.

### 4. Run Commands in the Recovery Terminal

Confirm that the current Terminal window is running in Recovery Mode, then execute the following commands one by one:

```bash
csrutil disable
csrutil allow-research-guests enable
```

Select the system volume used for daily operation, usually **Macintosh HD**.

After the commands complete, you can verify the settings:

```bash
csrutil status
csrutil allow-research-guests status
```

Then select **Apple menu → Restart** in the upper-left corner.

### 5. Configure the AMFI Boot Arguments

After restarting into normal macOS, open Terminal and run:

```bash
sudo nvram boot-args="amfi_get_out_of_my_way=1 -v"
```

This command overwrites the existing `boot-args`. Therefore, you must save the original value beforehand.

Then restart the Mac.

### 6. Verify the Configuration After Restarting

After entering macOS again, run:

```bash
csrutil status
csrutil allow-research-guests status
sysctl -n kern.bootargs
```

Expected results:

- SIP shows `disabled`.
- Research Guests shows `enabled`.
- `kern.bootargs` contains:

```text
amfi_get_out_of_my_way=1 -v
```

## III. Build and Verify

### 1. Build the Program

Clone the project and initialize its submodules.

Run the following command from the project root directory:

```bash
make build
```

A successful build should display output similar to:

```text
=== Building vphone-cli ===
=== Signing with entitlements ===
signed OK
```

### 2. Run the Preflight Check

```bash
make boot_host_preflight
```

Pay particular attention to the following items in the output:

- `SIP:`
- `allow-research-guests:`
- `current kern.bootargs:`
- `Signed Release Binary`
- `Result`

If the Mac security settings have been configured correctly, the following conditions should be met:

- SIP is `disabled`.
- Research Guests is `enabled`.
- The boot arguments contain `amfi_get_out_of_my_way=1`.
- The `Signed Release Binary` test exits with code `0`.
- The output should not contain `exit 137`, `signal 9`, or `nested VM`.

## IV. Run the Complete Automated Deployment

### 1. Run the Setup Command from the Project Root Directory

```bash
make setup_machine EXP=1
```

This installs the experimental firmware variant, including jailbreak-related functionality.

The command performs the following steps in sequence:

1. Installs Homebrew dependencies.
2. Creates a Python virtual environment.
3. Builds the required tools and `vphone-cli`.
4. Creates a default 64 GB virtual disk.
5. Downloads the iPhone IPSW and cloudOS firmware.
6. Merges and patches the boot chain.
7. Starts the virtual machine in DFU mode.
8. Obtains SHSH data.
9. Restores the virtual iPhone.
10. Mounts the virtual disk offline and installs the CFW.
11. Performs first-boot initialization.

The default firmware combination is the project-provided and tested `iPhone17,3 / iOS 26.1 / 23B85`, together with the corresponding cloudOS source.

### 2. Important Notes During Deployment

- Do not allow the Mac to enter sleep mode.
- Do not close the MacBook display.
- Do not close the Terminal window running the setup process.
- Do not manually terminate `vphone-cli`, the restore process, or the virtual machine window.
- When prompted for a `sudo` password, enter the password of the current Mac administrator account.
- Firmware downloads and restoration may take a long time. As long as the Terminal continues to show progress, keep waiting.

### 3. First-Boot Procedure

If the script displays output similar to:

```text
press Enter to start VM
```

Press Enter once to start the virtual machine.

The script will later display:

```text
Press Enter once the VM is fully booted
```

Wait until one of the following conditions is met:

- The virtual iPhone has reached a stable system interface; or
- The Regular variant console displays the `bash-4.4#` prompt.

After confirming that the system is no longer continuously scrolling boot logs, press Enter. The script will send the first-boot initialization commands and wait for the virtual machine to shut down.

The final output should include:

```text
=== Done ===
Setup completed.
```

## V. Routine Startup

After deployment is complete, use the following commands each time you want to start the virtual iPhone:

```bash
cd path_to_project/vphone-cli
make boot
```

Under normal conditions, this opens the virtual iPhone window.

When exiting, prefer the project window's normal exit method or shut down the virtual device normally. Do not force-terminate the process while the virtual disk is being written.

## VI. Create an ECDSA Key for vphone

Dropbear currently supports ECDSA. Create a dedicated ECDSA P-521 key for `vphone` so that future SSH logins do not require a password.

Run the following command on the host Mac:

```bash
ssh-keygen \
  -t ecdsa \
  -b 521 \
  -f ~/.ssh/vphone_ecdsa \
  -N "" \
  -C "vphone-benchmark"
```

Check the virtual machine's IP address.

Obtain the IP address from the top of the iOS simulator interface. For example:

```text
192.168.64.7
```

Then install the public key. Replace the IP address in the command with the correct address:

```bash
ssh \
  -p 22222 \
  -o PreferredAuthentications=password \
  -o PubkeyAuthentication=no \
  root@192.168.64.7 \
  'PATH=/var/jb/usr/bin:/var/jb/bin:/iosbinpack64/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin; export PATH; umask 077; mkdir -p /var/root/.ssh; cat >> /var/root/.ssh/authorized_keys; chmod 700 /var/root/.ssh; chmod 600 /var/root/.ssh/authorized_keys' \
  < ~/.ssh/vphone_ecdsa.pub
```

According to the `vphone-cli` project configuration, the default password for `root` is:

```text
alpine
```

Verify passwordless login:

```bash
ssh \
  -i ~/.ssh/vphone_ecdsa \
  -p 22222 \
  -o IdentitiesOnly=yes \
  -o BatchMode=yes \
  root@192.168.64.7 \
  'echo "public key login OK"'
```

Expected output:

```text
public key login OK
```

If the key is needed by Benchmark Bridge, set the private-key path to:

```text
~/.ssh/vphone_ecdsa
```

## VII. Basic iOS Virtual Device Control

### 1. Operations over SSH

Log in to the iPhone over SSH:

```bash
ssh -p 22222 root@192.168.64.7
```

The EXP Procursus environment includes `uiopen`.

Open Settings:

```bash
uiopen -a Settings
```

Open an application by Bundle ID:

```bash
uiopen -b com.apple.Health
uiopen -b com.apple.Maps
```

Open a URL:

```bash
uiopen -u https://www.apple.com.cn
```

List registered applications and their Bundle IDs:

```bash
uicache -l
```

Search for a specific application:

```bash
uicache -l | grep -i safari
```

If `/var/jb` has not yet been created, the EXP first-boot initialization has not completed. Check it with:

```bash
ls -ld /var/jb
tail -100 /var/log/vphone_jb_setup.log
```

### 2. Operations Without SSH

Run the following commands from the root directory of the `vphone-cli` repository:

```bash
cd path_to_project/vphone-cli
```

Inject a Home button event:

```bash
printf '%s\n' \
  '{"t":"key","name":"home","screen":false}' |
  nc -U vm/vphone.sock
```

A successful request returns:

```json
{"ok":true}
```

This injects an actual Home HID event through `vphoned`.

You can use the same method for other hardware buttons, including Power, Volume Up, and Volume Down:

```bash
printf '%s\n' '{"t":"key","name":"power","screen":false}' |
  nc -U vm/vphone.sock

printf '%s\n' '{"t":"key","name":"volup","screen":false}' |
  nc -U vm/vphone.sock

printf '%s\n' '{"t":"key","name":"voldown","screen":false}' |
  nc -U vm/vphone.sock
```

#### Take a Screenshot

```bash
cd /path_to_project/vphone-cli

mkdir -p screenshot

shot_file="$PWD/screenshot/iphone-$(date '+%Y%m%d-%H%M%S').png"

printf '{"t":"screenshot","path":"%s"}\n' "$shot_file" |
  nc -U vm/vphone.sock >/dev/null
```

The generated file path will look similar to:

```text
/path_to_project/vphone-cli/screenshot/iphone-20260721-143052.png
```

#### Tap and Swipe

Tap a screen coordinate:

```bash
printf '%s\n' \
  '{"t":"tap","x":645,"y":1398,"screen":false}' |
  nc -U vm/vphone.sock
```

Swipe upward:

```bash
printf '%s\n' \
  '{"t":"swipe","x1":645,"y1":2400,"x2":645,"y2":900,"ms":300,"screen":false}' |
  nc -U vm/vphone.sock
```

The default screen coordinate dimensions are `1290 × 2796`.
