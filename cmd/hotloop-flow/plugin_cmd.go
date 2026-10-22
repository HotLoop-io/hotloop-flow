package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/plugin"
)

const pluginUsage = `usage: hotloop-flow plugin <command>

  keygen -out <name>                make a publisher key pair: <name>.key (keep it
                                    private) and <name>.pub (give it to operators)
  sign -key <name>.key <dir>        sign <dir>/plugin.wasm and <dir>/plugin.json,
                                    writing <dir>/plugin.sig
  verify [-trust <key>]... <dir>    run every check a server runs when it loads
                                    the plugin, and say why if it would refuse

A trusted key is "ed25519:<base64>", a PEM public key, or the path of a file
holding one. The server reads them from plugins.trustedKeys.
`

func cmdPlugin(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, pluginUsage)
		return errors.New("plugin needs a command")
	}
	switch args[0] {
	case "keygen":
		return cmdPluginKeygen(args[1:], stdout)
	case "sign":
		return cmdPluginSign(args[1:], stdout)
	case "verify":
		return cmdPluginVerify(args[1:], stdout)
	}
	fmt.Fprint(os.Stderr, pluginUsage)
	return fmt.Errorf("unknown plugin command %q", args[0])
}

func cmdPluginKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("plugin keygen", flag.ContinueOnError)
	out := fs.String("out", "", "write <out>.key and <out>.pub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("keygen needs -out <name>")
	}
	priv, pub, err := plugin.GenerateKey()
	if err != nil {
		return err
	}
	keyPath, pubPath := *out+".key", *out+".pub"
	// O_EXCL on both: replacing a publisher key with a new one would quietly
	// orphan every plugin signed with the old one.
	if err := writeNew(keyPath, priv, 0o600); err != nil {
		return err
	}
	if err := writeNew(pubPath, pub, 0o644); err != nil {
		return err
	}
	pk, err := plugin.ParsePublicKey(string(pub))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "private key  %s  (keep it out of git)\npublic key   %s\nkey id       %s\ntrust it     %s\n",
		keyPath, pubPath, plugin.KeyID(pk), plugin.PublicKeyText(pk))
	return nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cmdPluginSign(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("plugin sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "the private key, as written by keygen")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || fs.NArg() != 1 {
		return errors.New("usage: hotloop-flow plugin sign -key <name>.key <dir>")
	}
	dir := fs.Arg(0)
	raw, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	priv, err := plugin.ParsePrivateKey(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", *keyPath, err)
	}
	module, err := os.ReadFile(filepath.Join(dir, plugin.ModuleFile))
	if err != nil {
		return err
	}
	descriptor, err := os.ReadFile(filepath.Join(dir, plugin.DescriptorFile))
	if err != nil {
		return err
	}
	// A descriptor the server would refuse is not worth a signature.
	if _, err := plugin.ParseManifest(descriptor); err != nil {
		return err
	}
	sig, err := plugin.Sign(priv, module, descriptor)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.SignatureFile), append(sig, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s with key %s\n", dir, plugin.KeyID(priv.Public().(ed25519.PublicKey)))
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdPluginVerify(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("plugin verify", flag.ContinueOnError)
	var trusted multiFlag
	fs.Var(&trusted, "trust", "a key to trust: ed25519:<base64>, a PEM public key, or a file holding one (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hotloop-flow plugin verify [-trust <key>]... <dir>")
	}
	trust, err := plugin.LoadTrust(trusted)
	if err != nil {
		return err
	}
	p, err := plugin.Inspect(context.Background(), fs.Arg(0), plugin.Policy{Trust: trust, MaxMemoryMiB: 4095})
	if err != nil {
		fmt.Fprintf(stdout, "REFUSED %s: %v\n", fs.Arg(0), err)
		return exitCode(1)
	}
	asks := "nothing"
	if len(p.Needs) > 0 {
		names := make([]string, len(p.Needs))
		for i, n := range p.Needs {
			names[i] = string(n)
		}
		asks = strings.Join(names, ", ")
	}
	fmt.Fprintf(stdout, "OK %s %s, signed by key %s, asks for %s\n", p.Manifest.Name, p.Manifest.Version, p.SignedBy, asks)
	return nil
}
