package main

// `fornax rm` — delete a model's files: one id, several, or every one.

import (
	"flag"
	"fmt"
	"os"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

const rmUsage = "usage: fornax rm <model>…   (-all removes every installed model)"

func cmdRm(args []string) error {
	set := flag.NewFlagSet("rm", flag.ExitOnError)
	all := set.Bool("all", false, "every model with files on disk — the engine and runtimes stay")
	set.Usage = ui.UsageFunc(set, rmUsage)
	set.Parse(args)
	ids := set.Args()
	root := paths.Home()
	if *all {
		if len(ids) > 0 {
			return fmt.Errorf("%s", rmUsage)
		}
		// Anything with a directory, so a half-finished download goes too.
		for _, spec := range modelrt.AllSpecs(root) {
			if _, err := os.Stat(paths.ModelDir(root, spec)); err == nil {
				ids = append(ids, spec.ID)
			}
		}
		if len(ids) == 0 {
			fmt.Printf("%s nothing installed\n", ui.MarkIdle())
			return nil
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("%s", rmUsage)
	}
	var freed int64
	for _, id := range ids {
		bytes, err := removeModel(root, id)
		if err != nil {
			return err
		}
		freed += bytes
	}
	if len(ids) > 1 {
		fmt.Printf("freed %s\n", ui.Bold(ui.HumanSize(freed)))
	}
	return nil
}

func removeModel(root, id string) (int64, error) {
	spec := modelrt.Model(id)
	if spec == nil {
		return 0, modelrt.UnknownModel(id)
	}
	dir := paths.ModelDir(root, spec)
	custom := modelrt.IsCustom(root, spec.ID)
	if _, err := os.Stat(dir); os.IsNotExist(err) && !custom {
		fmt.Printf("%s %s is not installed\n", ui.MarkIdle(), spec.ID)
		return 0, nil
	}
	freed := modelrt.DirSize(dir)
	if err := os.RemoveAll(dir); err != nil {
		return 0, fmt.Errorf("could not remove %s: %w", spec.Name, err)
	}
	if custom {
		if err := modelrt.DropCustom(root, spec.ID); err != nil {
			return freed, fmt.Errorf("removed files but could not update %s: %w", modelrt.CustomFile, err)
		}
	}
	fmt.Printf("%s %s removed %s\n", ui.Green("✓"), spec.ID, ui.Dim("("+ui.HumanSize(freed)+")"))
	if spec.Runtime == catalog.Kev {
		fmt.Println(ui.Dim("note: the shared Hugging Face cache (~/.cache/huggingface) is left alone"))
	}
	return freed, nil
}
