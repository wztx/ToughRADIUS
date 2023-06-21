package cwmpconfig

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/app"
	"github.com/talkincode/toughradius/common"
	"github.com/talkincode/toughradius/common/web"
	"github.com/talkincode/toughradius/models"
	"github.com/talkincode/toughradius/webserver"
)

// Mikrotik 脚本管理

func InitRouter() {

	webserver.GET("/admin/cwmp/config/session", func(c echo.Context) error {
		return c.Render(http.StatusOK, "cwmp_config_session", nil)
	})

	webserver.GET("/admin/cwmp/config/session/options", func(c echo.Context) error {
		var data []models.CwmpConfigSession
		common.Must(app.GDB().Find(&data).Error)
		var opts = make([]web.JsonOptions, 0)
		for _, d := range data {
			opts = append(opts, web.JsonOptions{
				Id:    strconv.FormatInt(d.ID, 10),
				Value: d.Name,
			})
		}
		return c.JSON(http.StatusOK, opts)
	})

	webserver.GET("/admin/cwmp/config/session/backup/:id", func(c echo.Context) error {
		var localfile = path.Join(app.GConfig().System.Workdir, fmt.Sprintf("supervise/%s.backup", c.Param("id")))
		return c.File(localfile)
	})

	webserver.GET("/admin/cwmp/config/session/autolog/:id", func(c echo.Context) error {
		var localfile = path.Join(app.GConfig().System.Workdir, fmt.Sprintf("supervise/%s.auto.log", c.Param("id")))
		return c.File(localfile)
	})

	webserver.GET("/admin/cwmp/config/session/query", queryCwmpConfigSession)

	webserver.ApiGET("/api/cwmp/config/session/query", queryCwmpConfigSession)

	webserver.POST("/admin/cwmp/config/session/execute", func(c echo.Context) error {
		var item models.CwmpConfigSession
		common.Must(app.GDB().Where("id=?", c.QueryParam("id")).First(&item).Error)
		webserver.PubOpLog(c, fmt.Sprintf("Re-execute the CWMP script session：%d: %s", item.ID, item.Name))
		return c.JSON(http.StatusOK, web.RestSucc("success"))
	})

	webserver.GET("/admin/cwmp/config/session/delete", func(c echo.Context) error {
		ids := c.QueryParam("ids")
		common.Must(app.GDB().Delete(models.CwmpConfigSession{}, strings.Split(ids, ",")).Error)
		webserver.PubOpLog(c, fmt.Sprintf("Delete CWMP script session information：%s", ids))
		return c.JSON(http.StatusOK, web.RestSucc("success"))
	})

	initTemplateRouter()

}

//	@Summary		Query cwmp config sessions
//	@Description	Query cwmp config sessions
//	@Tags			TR069
//	@Accept			json
//	@Produce		json
//	@Param			cpe_id	query	string	false	"cpe_id"
//	@Param			keyword	query	string	false	"keyword"
//	@Security		BearerAuth
//	@Success		200	{array}	models.CwmpConfigSession
//	@Router			/api/cwmp/config/session/query [get]
func queryCwmpConfigSession(c echo.Context) error {
	prequery := web.NewPreQuery(c).
		DefaultOrderBy("name asc").
		DateRange2("starttime", "endtime", "created_at", time.Now().Add(-time.Hour*24), time.Now()).
		QueryField("cpe_id", "cpe_id").
		KeyFields("name", "software_version", "product_class", "oui", "task_tags")

	result, err := web.QueryPageResult[models.CwmpConfigSession](c, app.GDB(), prequery)
	if err != nil {
		return c.JSON(http.StatusOK, common.EmptyList)
	}
	return c.JSON(http.StatusOK, result)
}
