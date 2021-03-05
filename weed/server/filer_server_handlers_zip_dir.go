package weed_server

import (
	"archive/zip"
	"context"
	"github.com/chrislusf/seaweedfs/weed/filer"
	"net/http"
	"strings"
)

const FetchCount = 100
func (fs *FilerServer) zipDirHandler(w http.ResponseWriter, entry *filer.Entry) error{

	zipFile := zip.NewWriter(w)

	defer zipFile.Close()

	return fs.doZipDirRecursion(zipFile, entry)

}

func (fs *FilerServer) doZipDirRecursion(file *zip.Writer, parent *filer.Entry) error {
	return fs.doZipDir(context.Background(), file, parent, string(parent.FullPath), "")
}

func (fs *FilerServer) doZipDir(ctx context.Context, file *zip.Writer, parent *filer.Entry, shouldCutPathPrefix string, startFilename string) error {

	entries, shouldDisplayLoadMore, err := fs.filer.ListDirectoryEntries(ctx, parent.FullPath, startFilename, false, int64(FetchCount), "", "")

	if err == nil {
		for _, entry := range entries {
			if entry.IsDirectory() {
				_ = fs.doZipDir(ctx, file, entry, shouldCutPathPrefix, "")
			}else {

				//去掉topics系统文件夹
				if !strings.HasSuffix(string(entry.FullPath), filer.SystemLogDir) {

					cutFullname := strings.TrimPrefix(string(entry.FullPath), shouldCutPathPrefix)
					zipEntryWriter, _ := file.Create(cutFullname)
					err = filer.StreamContent(fs.filer.MasterClient, zipEntryWriter, entry.Chunks, 0, int64(entry.Size()))

					if err != nil {
						return err
					}
				}

			}
		}
	}else {
		return err
	}

	if shouldDisplayLoadMore {

		startFilename = entries[len(entries) - 1].Name()
		err = fs.doZipDir(ctx, file, parent, shouldCutPathPrefix, startFilename)

		if err != nil {
			return err
		}
	}

	return nil
}