package webassets

import "io/fs"

func Files() fs.FS {
	assets, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err)
	}
	return assets
}
