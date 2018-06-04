package install

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"crawshaw.io/sqlite"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/itchio/butler/butlerd"
	"github.com/itchio/butler/butlerd/messages"
	"github.com/itchio/butler/cmd/operate"
	"github.com/itchio/butler/database/models"
	"github.com/itchio/butler/endpoints/downloads"
	itchio "github.com/itchio/go-itchio"
	"github.com/itchio/wharf/state"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
)

func InstallQueue(rc *butlerd.RequestContext, queueParams *butlerd.InstallQueueParams) (*butlerd.InstallQueueResult, error) {
	var stagingFolder string

	var cave *models.Cave
	var installLocation *models.InstallLocation

	reason := queueParams.Reason
	if reason == "" {
		reason = butlerd.DownloadReasonInstall
	}

	var id string
	if queueParams.NoCave {
		if queueParams.StagingFolder == "" {
			return nil, errors.New("With noCave, installFolder must be specified")
		}
		stagingFolder = queueParams.StagingFolder
		uu, err := uuid.NewV4()
		if err != nil {
			panic(err)
		}
		id = uu.String()
	} else {
		if queueParams.CaveID == "" {
			if queueParams.InstallLocationID == "" {
				return nil, errors.New("When caveId is unspecified, installLocationId must be set")
			}
			rc.WithConn(func(conn *sqlite.Conn) {
				installLocation = models.InstallLocationByID(conn, queueParams.InstallLocationID)
			})
			if installLocation == nil {
				return nil, errors.Errorf("Install location not found (%s)", queueParams.InstallLocationID)
			}
		} else {
			cave = operate.ValidateCave(rc, queueParams.CaveID)
			if queueParams.Game == nil {
				queueParams.Game = cave.Game
			}
			if queueParams.Upload == nil {
				queueParams.Upload = cave.Upload
			}
			if queueParams.Build == nil {
				queueParams.Build = cave.Build
			}

			rc.WithConn(func(conn *sqlite.Conn) {
				installLocation = cave.GetInstallLocation(conn)
			})
		}

		id = generateDownloadID(installLocation.Path)
		stagingFolder = installLocation.GetStagingFolder(id)
	}

	oc, err := operate.LoadContext(rc.Ctx, rc, stagingFolder)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer oc.Release()

	consumer := oc.Consumer()
	meta := operate.NewMetaSubcontext()
	params := meta.Data

	params.StagingFolder = stagingFolder
	params.Reason = reason

	if queueParams.Game == nil {
		return nil, errors.New("Missing game in install")
	}

	params.Game = queueParams.Game
	rc.WithConn(func(conn *sqlite.Conn) {
		params.Access = operate.AccessForGameID(conn, params.Game.ID)
	})

	client := rc.Client(params.Access.APIKey)

	{
		// attempt to refresh game info
		gameRes, err := client.GetGame(itchio.GetGameParams{
			GameID:      params.Game.ID,
			Credentials: params.Access.Credentials,
		})
		if err != nil {
			consumer.Warnf("Could not refresh game info: %s", err.Error())
		} else {
			params.Game = gameRes.Game
		}
	}

	if queueParams.NoCave {
		if queueParams.InstallFolder == "" {
			return nil, errors.New("With noCave is specified, InstallFolder cannot be empty")
		}

		params.NoCave = true
		params.InstallFolder = queueParams.InstallFolder
	} else {
		if cave == nil {
			freshCaveID, err := uuid.NewV4()
			if err != nil {
				return nil, errors.WithStack(err)
			}

			cave = &models.Cave{
				ID:                freshCaveID.String(),
				InstallLocationID: queueParams.InstallLocationID,
			}
		}
		params.CaveID = cave.ID

		rc.WithConn(func(conn *sqlite.Conn) {
			if cave.InstallFolderName == "" {
				cave.InstallFolderName = makeInstallFolderName(params.Game, consumer)
				ensureUniqueFolderName(conn, cave)
			}

			params.InstallFolder = cave.GetInstallFolder(conn)
			params.InstallLocationID = cave.InstallLocationID
			params.InstallFolderName = cave.InstallFolderName
		})
	}

	params.Upload = queueParams.Upload
	params.Build = queueParams.Build

	if params.Upload == nil {
		consumer.Infof("No upload specified, looking for compatible ones...")
		uploadsFilterResult, err := operate.GetFilteredUploads(client, params.Game, params.Access.Credentials, consumer)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if len(uploadsFilterResult.Uploads) == 0 {
			consumer.Errorf("Didn't find a compatible upload.")
			consumer.Errorf("The initial %d uploads were:", len(uploadsFilterResult.InitialUploads))
			for _, upload := range uploadsFilterResult.InitialUploads {
				operate.LogUpload(consumer, upload, upload.Build)
			}

			return nil, errors.WithStack(butlerd.CodeNoCompatibleUploads)
		}

		if len(uploadsFilterResult.Uploads) == 1 {
			params.Upload = uploadsFilterResult.Uploads[0]
		} else {
			r, err := messages.PickUpload.Call(rc, &butlerd.PickUploadParams{
				Uploads: uploadsFilterResult.Uploads,
			})
			if err != nil {
				return nil, errors.WithStack(err)
			}

			if r.Index < 0 {
				return nil, errors.WithStack(butlerd.CodeOperationAborted)
			}

			params.Upload = uploadsFilterResult.Uploads[r.Index]
		}

		if params.Upload.Build != nil {
			// if we reach this point, we *just now* queried for an upload,
			// so we know the build object is the latest
			params.Build = params.Upload.Build
		}
	}

	// params.Upload can't be nil by now
	if params.Build == nil {
		// We were passed an upload but not a build:
		// Let's refresh upload info so we can settle on a build we want to install (if any)

		listUploadsRes, err := client.ListGameUploads(itchio.ListGameUploadsParams{
			GameID:      params.Game.ID,
			Credentials: params.Access.Credentials,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}

		found := true
		for _, u := range listUploadsRes.Uploads {
			if u.ID == params.Upload.ID {
				if u.Build == nil {
					consumer.Infof("Upload is not wharf-enabled")
				} else {
					consumer.Infof("Latest build for upload is %d", u.Build.ID)
					params.Build = u.Build
				}
				break
			}
		}

		if !found {
			consumer.Errorf("Uh oh, we didn't find that upload on the server:")
			operate.LogUpload(consumer, params.Upload, nil)
			return nil, errors.New("Upload not found")
		}
	}

	if operate.UploadIsProbablyExternal(params.Upload) {
		res, err := messages.ExternalUploadsAreBad.Call(rc, &butlerd.ExternalUploadsAreBadParams{
			Upload: params.Upload,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if res.Whatever {
			// let's keep going then.
		} else {
			return nil, errors.WithStack(butlerd.CodeOperationAborted)
		}
	}

	oc.Save(meta)

	istate := &operate.InstallSubcontextState{}
	isub := &operate.InstallSubcontext{
		Data: istate,
	}
	oc.Load(isub)

	err = operate.InstallPrepare(oc, meta, isub, false /* disallow downloads */, func(res *operate.InstallPrepareResult) error {
		// do muffin
		return nil
	})
	if err != nil {
		oc.Retire()
		return nil, errors.WithStack(err)
	}

	res := &butlerd.InstallQueueResult{
		ID:            id,
		CaveID:        params.CaveID,
		Game:          params.Game,
		Upload:        params.Upload,
		Build:         params.Build,
		InstallFolder: params.InstallFolder,
		StagingFolder: params.StagingFolder,
		Reason:        params.Reason,
	}

	if queueParams.QueueDownload {
		_, err := downloads.DownloadsQueue(rc, &butlerd.DownloadsQueueParams{
			Item: res,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return res, nil
}

func makeInstallFolderName(game *itchio.Game, consumer *state.Consumer) string {
	name := makeInstallFolderNameFromSlug(game, consumer)
	if name == "" {
		name = makeInstallFolderNameFromID(game, consumer)
	}
	return name
}

var slugRe = regexp.MustCompile(`^\/([^\/]+)`)

func makeInstallFolderNameFromSlug(game *itchio.Game, consumer *state.Consumer) string {
	if game.URL == "" {
		return ""
	}

	u, err := url.Parse(game.URL)
	if err != nil {
		consumer.Warnf("Could not parse game URL (%s): %s", game.URL, err.Error())
		return ""
	}

	matches := slugRe.FindStringSubmatch(u.Path)
	if len(matches) == 2 {
		return matches[1]
	}

	return ""
}

func makeInstallFolderNameFromID(game *itchio.Game, consumer *state.Consumer) string {
	return fmt.Sprintf("game-%d", game.ID)
}

func ensureUniqueFolderName(conn *sqlite.Conn, cave *models.Cave) {
	// Once we reach "Overland 200", it's time to stop
	const uniqueMaxTries = 200
	base := cave.InstallFolderName
	suffix := 2

	for i := 0; i < uniqueMaxTries; i++ {
		folder := cave.GetInstallFolder(conn)
		_, err := os.Stat(folder)
		alreadyExists := (err == nil)

		if !alreadyExists {
			// coolio
			return
		}

		// uh oh, it exists
		cave.InstallFolderName = fmt.Sprintf("%s %d", base, suffix)
		suffix++
	}

	cave.InstallFolderName = base
	err := errors.Errorf("Could not ensure unique install folder starting with (%s)", cave.GetInstallFolder(conn))
	panic(err)
}

func generateDownloadID(basePath string) string {
	for tries := 100; tries > 0; tries-- {
		id := petname.Generate(3, "-")
		_, err := os.Stat(filepath.Join(basePath, id))
		if err != nil && os.IsNotExist(err) {
			return id
		}
	}

	uu, err := uuid.NewV4()
	if err != nil {
		// i know watcha thinking - ooh, ah, don't panic!
		// well, if we reach here, we don't have a good enough random
		// source to generate an UUID, and we've also tried a hundred
		// combination of cute animal names, so I don't know what to
		// tell you. butlerd handlers catch panics anyway!
		panic(err)
	}
	return uu.String()
}
