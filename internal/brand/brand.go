// Package brand — единственный источник имени продукта в коде.
//
// GamayunNas — имя продукта. Пользовательские строки
// (интерфейс, сообщения, заголовки) берут имя только отсюда; технические
// идентификаторы — gamayun-nas, gamayund, gamayun-helper.
package brand

// Name — отображаемое имя продукта. Можно переопределить при сборке:
//
//	go build -ldflags "-X gamayun-nas/internal/brand.Name=..." ./cmd/...
var Name = "GamayunNas"
