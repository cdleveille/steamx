# steamx

A small tray daemon for Linux that switches a **Steam Controller (2026)** between:

- **Steam mode** (default): the controller is left alone and works as usual.
- **XInput mode**: the Steam Controller is hidden from Steam and games and replaced
  by a virtual wired **Xbox 360 controller** (`045e:028e`, "Microsoft X-Box 360 pad").
  Steam and games see a regular Xbox 360 controller, and Steam doesn't need to be running.

| Steam Controller | Xbox 360 controller |
| --- | --- |
| A / B / X / Y, bumpers, analog triggers | A / B / X / Y, LB / RB, LT / RT |
| Sticks and stick clicks, D-pad | Sticks, L3 / R3, D-pad |
| View / Menu / Steam button | Back / Start / Guide |
| Rumble | Rumble |
| Trackpads, grip buttons, `…` button, gyro | Disabled |

It works with the USB cable, the wireless puck (also the Steam Machine's built-in
receiver) and Bluetooth. Controllers that connect while XInput mode is on are picked up
automatically.

## Requirements

- 64-bit Linux with systemd. The release binary is for x86_64; on arm64, build from
  source with Go 1.22 or newer.
- Steam's udev rules. They give your user access to `/dev/uinput` and the controller.
  On CachyOS/Arch, install them with `sudo pacman -S steam-devices`. Other distros have
  a package of the same name, and it's usually already installed with Steam.
- A system tray with StatusNotifierItem support. KDE Plasma supports it natively. On
  GNOME, add the "AppIndicator and KStatusNotifierItem Support" extension.

## Install

From the repository root:

```sh
./install.sh
```

The script asks how to get the binary: download the latest release from GitHub (the
default, needs `curl`) or build it from source (needs Go 1.22 or newer). It then installs
it to `~/.local/bin/steamx`, and creates, enables and starts the systemd user service
`steamx.service`. From then on, steamx runs in the background and starts automatically
with your desktop session. Re-run `./install.sh` to update; choose to build from
source to install local changes.

To remove the service and the binary:

```sh
./uninstall.sh
```

Useful service commands:

```sh
systemctl --user status steamx    # is it running?
systemctl --user start steamx     # start it again after using Quit in the tray menu
journalctl --user -u steamx -f    # follow its log
```

## Build manually

To build without installing, from the repository root:

```sh
CGO_ENABLED=0 go build -C src -trimpath -ldflags="-s -w" -o ../dist/steamx .
```

The result is a single, statically linked `dist/steamx` binary with no runtime
dependencies.

## Use

- **Left-click** the tray icon, or right-click it and use the **XInput mode** switch.
  A blue icon means Steam mode and a green icon means XInput mode. The tooltip shows
  how many controllers are being emulated.
- **Quit** (or logging out) puts the controller back in Steam mode. If steamx crashes
  or is killed, systemd restarts it, which also restores the controller. Re-plugging
  the controller restores it too.

## Notes

- **Bluetooth while Steam is running:** without root access, a Bluetooth controller
  can't be hidden from Steam. Steam would see the Steam Controller *and* the virtual
  Xbox controller. Use the cable or the puck while Steam is running, or close Steam.
  Over USB and the puck, the Steam Controller is fully hidden.
- If *Steam Settings → Controller → Enable Steam Input for Xbox controllers* is on,
  Steam treats the virtual controller like any real Xbox controller and applies Steam
  Input to it in Steam games. Turn that setting off if you want plain XInput passthrough.
