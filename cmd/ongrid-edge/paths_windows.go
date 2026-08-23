//go:build windows

package main

// Windows default path constants. The directories are created by the
// supervisor.exe --install subcommand.
//
// Layout:
//   - binary → C:\Program Files\ongrid-edge\bin\
//   - data   → C:\ProgramData\ongrid-edge\
//
// ProgramData is the standard data directory for Windows services and is
// writable by the service account running the edge.

func defaultPluginBinDir() string { return `C:\Program Files\ongrid-edge\bin` }

func defaultPluginWorkDir() string { return `C:\ProgramData\ongrid-edge\plugins` }

func defaultStageDir() string { return `C:\ProgramData\ongrid-edge\upgrade` }

// defaultSecretsFile returns the default path of the DPAPI-encrypted
// secrets.enc, placed under ProgramData (writable by the service account).
func defaultSecretsFile() string { return `C:\ProgramData\ongrid-edge\secrets.enc` }
