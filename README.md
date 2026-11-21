# keplerGO

Monitor and overclock Nvidia Kepler GPUs (Quadro K-series, Tesla K20/K40/K80) on Debian with the legacy 470.xx driver. Go standard library only.

## Build

    sudo apt install golang-go
    go build -o keplergo .
    go test ./...
    bash tests/smoke.sh        # safety net check with fake nvidia tools, no GPU touched
    sudo install -m755 keplergo /usr/local/bin/

## Driver (Kepler)

Kepler needs the legacy 470.xx branch. On Debian 13 install
`nvidia-tesla-470-driver` and `nvidia-settings-tesla-470`
(check `apt-cache policy nvidia-tesla-470-driver` first).
Do not install plain `nvidia-settings` or `nvidia-driver`: they pull the 550
branch, which does not support Kepler.

## Commands

    keplergo info                         GPU details, capabilities, supported clocks
    keplergo status                       active session, clocks, offsets
    keplergo watch                        live monitor
    keplergo oc -gfx 30 -mem 100          clock offsets (Xorg + Coolbits, run as your user)
    keplergo tune -step 10 -max 100       find a stable core offset step by step
    sudo keplergo apply -mem 3004 -gfx 875   application clocks (Tesla / desktop Kepler)
    keplergo otval                        restore everything to defaults
    keplergo debug > debug.txt            diagnostics after a failed attempt

## Safety model

- A watchdog is started in its own session before anything is applied.
- No ENTER within the window (default 20 s), a closed terminal, heat, throttling
  or an unresponsive nvidia-smi restore the defaults automatically.
- otval works from a text console (Ctrl+Alt+F3) or over ssh; it reuses the
  DISPLAY and XAUTHORITY saved by the session that applied the overclock.
- Nothing is written to the system and nothing starts at boot. A reboot always
  returns the card to stock.
- Out of scope on purpose: VBIOS flashing, voltage, fan control.

Offsets: core -300..150 MHz, memory -500..400 MT/s, max-temp 40..95 C (`-unsafe`
lifts the offset limits, never the temperature limit).
Exit codes: 0 ok, 1 error, 2 usage, 3 defaults were restored.
State and log: ~/.local/state/keplergo (session.json, events.log).
