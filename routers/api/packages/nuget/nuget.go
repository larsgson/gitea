// Copyright 2021 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package nuget

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"code.gitea.io/gitea/models/db"
	packages_model "code.gitea.io/gitea/models/packages"
	"code.gitea.io/gitea/modules/context"
	packages_module "code.gitea.io/gitea/modules/packages"
	nuget_module "code.gitea.io/gitea/modules/packages/nuget"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/routers/api/packages/helper"
	packages_service "code.gitea.io/gitea/services/packages"
)

func apiError(ctx *context.Context, status int, obj interface{}) {
	helper.LogAndProcessError(ctx, status, obj, func(message string) {
		ctx.JSON(status, map[string]string{
			"Message": message,
		})
	})
}

// ServiceIndex https://docs.microsoft.com/en-us/nuget/api/service-index
func ServiceIndex(ctx *context.Context) {
	resp := createServiceIndexResponse(setting.AppURL + "api/packages/" + ctx.Package.Owner.Name + "/nuget")

	ctx.JSON(http.StatusOK, resp)
}

// SearchService https://docs.microsoft.com/en-us/nuget/api/search-query-service-resource#search-for-packages
func SearchService(ctx *context.Context) {
	pvs, count, err := packages_model.SearchVersions(ctx, &packages_model.PackageSearchOptions{
		OwnerID:    ctx.Package.Owner.ID,
		Type:       packages_model.TypeNuGet,
		Name:       packages_model.SearchValue{Value: ctx.FormTrim("q")},
		IsInternal: util.OptionalBoolFalse,
		Paginator: db.NewAbsoluteListOptions(
			ctx.FormInt("skip"),
			ctx.FormInt("take"),
		),
	})
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	pds, err := packages_model.GetPackageDescriptors(ctx, pvs)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	resp := createSearchResultResponse(
		&linkBuilder{setting.AppURL + "api/packages/" + ctx.Package.Owner.Name + "/nuget"},
		count,
		pds,
	)

	ctx.JSON(http.StatusOK, resp)
}

// RegistrationIndex https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#registration-index
func RegistrationIndex(ctx *context.Context) {
	packageName := ctx.Params("id")

	pvs, err := packages_model.GetVersionsByPackageName(ctx, ctx.Package.Owner.ID, packages_model.TypeNuGet, packageName)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}
	if len(pvs) == 0 {
		apiError(ctx, http.StatusNotFound, err)
		return
	}

	pds, err := packages_model.GetPackageDescriptors(ctx, pvs)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	resp := createRegistrationIndexResponse(
		&linkBuilder{setting.AppURL + "api/packages/" + ctx.Package.Owner.Name + "/nuget"},
		pds,
	)

	ctx.JSON(http.StatusOK, resp)
}

// RegistrationLeaf https://docs.microsoft.com/en-us/nuget/api/registration-base-url-resource#registration-leaf
func RegistrationLeaf(ctx *context.Context) {
	packageName := ctx.Params("id")
	packageVersion := strings.TrimSuffix(ctx.Params("version"), ".json")

	pv, err := packages_model.GetVersionByNameAndVersion(ctx, ctx.Package.Owner.ID, packages_model.TypeNuGet, packageName, packageVersion)
	if err != nil {
		if err == packages_model.ErrPackageNotExist {
			apiError(ctx, http.StatusNotFound, err)
			return
		}
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	pd, err := packages_model.GetPackageDescriptor(ctx, pv)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	resp := createRegistrationLeafResponse(
		&linkBuilder{setting.AppURL + "api/packages/" + ctx.Package.Owner.Name + "/nuget"},
		pd,
	)

	ctx.JSON(http.StatusOK, resp)
}

// EnumeratePackageVersions https://docs.microsoft.com/en-us/nuget/api/package-base-address-resource#enumerate-package-versions
func EnumeratePackageVersions(ctx *context.Context) {
	packageName := ctx.Params("id")

	pvs, err := packages_model.GetVersionsByPackageName(ctx, ctx.Package.Owner.ID, packages_model.TypeNuGet, packageName)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}
	if len(pvs) == 0 {
		apiError(ctx, http.StatusNotFound, err)
		return
	}

	pds, err := packages_model.GetPackageDescriptors(ctx, pvs)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	resp := createPackageVersionsResponse(pds)

	ctx.JSON(http.StatusOK, resp)
}

// DownloadPackageFile https://docs.microsoft.com/en-us/nuget/api/package-base-address-resource#download-package-content-nupkg
func DownloadPackageFile(ctx *context.Context) {
	packageName := ctx.Params("id")
	packageVersion := ctx.Params("version")
	filename := ctx.Params("filename")

	s, pf, err := packages_service.GetFileStreamByPackageNameAndVersion(
		ctx,
		&packages_service.PackageInfo{
			Owner:       ctx.Package.Owner,
			PackageType: packages_model.TypeNuGet,
			Name:        packageName,
			Version:     packageVersion,
		},
		&packages_service.PackageFileInfo{
			Filename: filename,
		},
	)
	if err != nil {
		if err == packages_model.ErrPackageNotExist || err == packages_model.ErrPackageFileNotExist {
			apiError(ctx, http.StatusNotFound, err)
			return
		}
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}
	defer s.Close()

	ctx.ServeStream(s, pf.Name)
}

// UploadPackage creates a new package with the metadata contained in the uploaded nupgk file
// https://docs.microsoft.com/en-us/nuget/api/package-publish-resource#push-a-package
func UploadPackage(ctx *context.Context) {
	np, buf, closables := processUploadedFile(ctx, nuget_module.DependencyPackage)
	defer func() {
		for _, c := range closables {
			c.Close()
		}
	}()
	if np == nil {
		return
	}

	_, _, err := packages_service.CreatePackageAndAddFile(
		&packages_service.PackageCreationInfo{
			PackageInfo: packages_service.PackageInfo{
				Owner:       ctx.Package.Owner,
				PackageType: packages_model.TypeNuGet,
				Name:        np.ID,
				Version:     np.Version,
			},
			SemverCompatible: true,
			Creator:          ctx.Doer,
			Metadata:         np.Metadata,
		},
		&packages_service.PackageFileCreationInfo{
			PackageFileInfo: packages_service.PackageFileInfo{
				Filename: strings.ToLower(fmt.Sprintf("%s.%s.nupkg", np.ID, np.Version)),
			},
			Data:   buf,
			IsLead: true,
		},
	)
	if err != nil {
		if err == packages_model.ErrDuplicatePackageVersion {
			apiError(ctx, http.StatusConflict, err)
			return
		}
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	ctx.Status(http.StatusCreated)
}

// UploadSymbolPackage adds a symbol package to an existing package
// https://docs.microsoft.com/en-us/nuget/api/symbol-package-publish-resource
func UploadSymbolPackage(ctx *context.Context) {
	np, buf, closables := processUploadedFile(ctx, nuget_module.SymbolsPackage)
	defer func() {
		for _, c := range closables {
			c.Close()
		}
	}()
	if np == nil {
		return
	}

	pdbs, err := nuget_module.ExtractPortablePdb(buf, buf.Size())
	if err != nil {
		apiError(ctx, http.StatusBadRequest, err)
		return
	}
	defer pdbs.Close()

	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}

	pi := &packages_service.PackageInfo{
		Owner:       ctx.Package.Owner,
		PackageType: packages_model.TypeNuGet,
		Name:        np.ID,
		Version:     np.Version,
	}

	_, _, err = packages_service.AddFileToExistingPackage(
		pi,
		&packages_service.PackageFileCreationInfo{
			PackageFileInfo: packages_service.PackageFileInfo{
				Filename: strings.ToLower(fmt.Sprintf("%s.%s.snupkg", np.ID, np.Version)),
			},
			Data:   buf,
			IsLead: false,
		},
	)
	if err != nil {
		switch err {
		case packages_model.ErrPackageNotExist:
			apiError(ctx, http.StatusNotFound, err)
		case packages_model.ErrDuplicatePackageFile:
			apiError(ctx, http.StatusConflict, err)
		default:
			apiError(ctx, http.StatusInternalServerError, err)
		}
		return
	}

	for _, pdb := range pdbs {
		_, _, err := packages_service.AddFileToExistingPackage(
			pi,
			&packages_service.PackageFileCreationInfo{
				PackageFileInfo: packages_service.PackageFileInfo{
					Filename:     strings.ToLower(pdb.Name),
					CompositeKey: strings.ToLower(pdb.ID),
				},
				Data:   pdb.Content,
				IsLead: false,
				Properties: map[string]string{
					nuget_module.PropertySymbolID: strings.ToLower(pdb.ID),
				},
			},
		)
		if err != nil {
			switch err {
			case packages_model.ErrDuplicatePackageFile:
				apiError(ctx, http.StatusConflict, err)
			default:
				apiError(ctx, http.StatusInternalServerError, err)
			}
			return
		}
	}

	ctx.Status(http.StatusCreated)
}

func processUploadedFile(ctx *context.Context, expectedType nuget_module.PackageType) (*nuget_module.Package, *packages_module.HashedBuffer, []io.Closer) {
	closables := make([]io.Closer, 0, 2)

	upload, close, err := ctx.UploadStream()
	if err != nil {
		apiError(ctx, http.StatusBadRequest, err)
		return nil, nil, closables
	}

	if close {
		closables = append(closables, upload)
	}

	buf, err := packages_module.CreateHashedBufferFromReader(upload, 32*1024*1024)
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return nil, nil, closables
	}
	closables = append(closables, buf)

	np, err := nuget_module.ParsePackageMetaData(buf, buf.Size())
	if err != nil {
		if err == nuget_module.ErrMissingNuspecFile || err == nuget_module.ErrNuspecFileTooLarge || err == nuget_module.ErrNuspecInvalidID || err == nuget_module.ErrNuspecInvalidVersion {
			apiError(ctx, http.StatusBadRequest, err)
		} else {
			apiError(ctx, http.StatusInternalServerError, err)
		}
		return nil, nil, closables
	}
	if np.PackageType != expectedType {
		apiError(ctx, http.StatusBadRequest, errors.New("unexpected package type"))
		return nil, nil, closables
	}
	if _, err := buf.Seek(0, io.SeekStart); err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return nil, nil, closables
	}
	return np, buf, closables
}

// DownloadSymbolFile https://github.com/dotnet/symstore/blob/main/docs/specs/Simple_Symbol_Query_Protocol.md#request
func DownloadSymbolFile(ctx *context.Context) {
	filename := ctx.Params("filename")
	guid := ctx.Params("guid")
	filename2 := ctx.Params("filename2")

	if filename != filename2 {
		apiError(ctx, http.StatusBadRequest, nil)
		return
	}

	pfs, _, err := packages_model.SearchFiles(ctx, &packages_model.PackageFileSearchOptions{
		OwnerID:     ctx.Package.Owner.ID,
		PackageType: string(packages_model.TypeNuGet),
		Query:       filename,
		Properties: map[string]string{
			nuget_module.PropertySymbolID: strings.ToLower(guid),
		},
	})
	if err != nil {
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}
	if len(pfs) != 1 {
		apiError(ctx, http.StatusNotFound, nil)
		return
	}

	s, _, err := packages_service.GetPackageFileStream(ctx, pfs[0])
	if err != nil {
		if err == packages_model.ErrPackageNotExist || err == packages_model.ErrPackageFileNotExist {
			apiError(ctx, http.StatusNotFound, err)
			return
		}
		apiError(ctx, http.StatusInternalServerError, err)
		return
	}
	defer s.Close()

	ctx.ServeStream(s, pfs[0].Name)
}

// DeletePackage hard deletes the package
// https://docs.microsoft.com/en-us/nuget/api/package-publish-resource#delete-a-package
func DeletePackage(ctx *context.Context) {
	packageName := ctx.Params("id")
	packageVersion := ctx.Params("version")

	err := packages_service.RemovePackageVersionByNameAndVersion(
		ctx.Doer,
		&packages_service.PackageInfo{
			Owner:       ctx.Package.Owner,
			PackageType: packages_model.TypeNuGet,
			Name:        packageName,
			Version:     packageVersion,
		},
	)
	if err != nil {
		if err == packages_model.ErrPackageNotExist {
			apiError(ctx, http.StatusNotFound, err)
			return
		}
		apiError(ctx, http.StatusInternalServerError, err)
	}

	ctx.Status(http.StatusNoContent)
}
