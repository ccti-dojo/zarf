## zarf tools clear-cache

Clears the configured git and image cache directory

```
zarf tools clear-cache [flags]
```

### Options

```
  -h, --help                help for clear-cache
      --zarf-cache string   Specify the location of the Zarf  artifact cache (images and git repositories) (default "~/.zarf-cache")
```

### Options inherited from parent commands

```
  -a, --architecture string   Architecture for OCI images
  -l, --log-level string      Log level when running Zarf. Valid options are: warn, info, debug, trace (default "info")
      --no-log-file           Disable log file creation
      --no-progress           Disable fancy UI progress bars, spinners, logos, etc
      --tmpdir string         Specify the temporary directory to use for intermediate files
```

### SEE ALSO

* [zarf tools](zarf_tools.md)	 - Collection of additional tools to make airgap easier

