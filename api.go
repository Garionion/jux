package main

import (
	"github.com/gofiber/cors"
	"github.com/gofiber/fiber"
	"github.com/gofiber/websocket"
	"github.com/google/uuid"
	"github.com/ilyakaznacheev/cleanenv"
	"gosrc.io/xmpp"
	"gosrc.io/xmpp/stanza"
	"io"
	"log"
	"os"
	"syscall"
)

type Client struct {
	sendChannel    chan WSMessage //from client to server
	receiveChannel chan WSMessage //from server to client
	xmppClient     *xmpp.Client
}

type MessageType int
const (
	Ctrl MessageType = iota
	Msg
)

type WSMessage struct {
	MessageType `json:"messageType"`
	*CtrlMessage `json:"ctrlMessage,omitempty"`
	*XMPPMessage `json:"xmppMessage,omitempty"`
}

type CtrlMessage struct {
	Action string `json:"action,omitempty"`
}

type XMPPMessage struct {
	From string		`json:"from,omitempty"`
	To string		`json:"to,omitempty"`
	Handle string	`json:"handle,omitempty"`
	Body string		`json:"body,omitempty"`
}

var clients = map[string]Client{}

type Config struct {
	XMPP struct {
		Address  string `env:"XMPP_ADDRESS" yaml:"address" env-default:"localhost:5432" env-description:"Server Address to connect to"`
		Domain   string `env:"XMPP_DOMAIN" yaml:"domain" env-required:"true" env-description:"Server Name of the XMPP Server (the server could serve for another domain than be reachable of)"`
		Jid      string `env:"JID" yaml:"jid" env-required:"true"`
		Password string `env:"XMPP_PASSWORD" yaml:"password" env-required:"true"`
	} `yaml:"xmpp"`
}

func main() {
	var cfg Config
	err := cleanenv.ReadConfig("config.yml", &cfg)
	if err != nil {
		log.Fatal("Config: " + err.Error())
	}

	
	debuglog, err := os.OpenFile("./debug.log", syscall.O_RDWR, 0)
	if err != nil {
		log.Print(err)
	}
	xmppConfig := xmpp.Config{
		TransportConfiguration: xmpp.TransportConfiguration{
			Address: cfg.XMPP.Address,
			Domain: cfg.XMPP.Domain,
		},
		Jid:          cfg.XMPP.Jid,
		Credential:   xmpp.Password(cfg.XMPP.Password),
		StreamLogger: debuglog,
		Insecure:     true,
		// TLSConfig: tls.Config{InsecureSkipVerify: true},
	}

	//TODO gc old ws connections

	app := fiber.New()
	app.Use(cors.New())

	//TODO use Ctrl Message via websocket to create new connection
	app.Post("/newConnection", func(c *fiber.Ctx) {
		id :=  clientHandler(&xmppConfig)
		log.Println("Got ID: ", id)
		c.SendString(id)
	})

	app.Use(func(c *fiber.Ctx) {
		// IsWebSocketUpgrade returns true if the client
		// requested upgrade to the WebSocket protocol.
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			c.Next()
		}
	})

	app.Get("/ws/:id", websocket.New(func(c *websocket.Conn) {

		// websocket.Conn bindings https://pkg.go.dev/github.com/fasthttp/websocket?tab=doc#pkg-index

		id := c.Params("id")
		client, ok := clients[id]
		if !ok {
			c.Close()
			return
		}
		log.Println("Connection for id: " + id)
		go func() {
			var message WSMessage
			var err error
			for  {
				if err = c.ReadJSON(&message); err != nil {
					log.Println("read:", err)
					if err == io.ErrUnexpectedEOF {
						//TODO log last connection
						break
					}
					continue
				}
				client.sendChannel <- message
				log.Printf("recv: %s", message)
			}
		}()
		for {
			msg := <- client.receiveChannel
			if err = c.WriteJSON(msg); err != nil {
				log.Println("write:", err)
				//TODO what errors can possibly occur
			}
		}

	}))


	log.Fatal(app.Listen(3000))
}

func genID() string {
	uid, err := uuid.NewUUID()
	if err != nil {
		log.Fatal("Couldn't generate UUID: " + err.Error())
	}
	return  uid.String()
}

func clientHandler(config *xmpp.Config) string {
	client := Client{
		sendChannel:    make(chan WSMessage),
		receiveChannel: make(chan WSMessage),
	}
	
	router := xmpp.NewRouter()
	router.HandleFunc("message", func (s xmpp.Sender, p stanza.Packet) {
		msg, ok := p.(stanza.Message)
		if !ok {
			log.Fatalf("Ignoring packet: %T\n", p)
			return
		}
		//TODO extract username
		message := WSMessage{
			MessageType: Msg,
			XMPPMessage: &XMPPMessage{
				From:   "",
				To:     config.Jid,
				Handle: msg.From,
				Body:   msg.Body,
			},
		}
		client.receiveChannel <- message
		
	})

	xmppClient, err := xmpp.NewClient(config, router, errorHandler)
	if err != nil {
		log.Fatalf("%+v", err)
	}
	client.xmppClient = xmppClient
	// If you pass the xmppClient to a connection manager, it will handle the reconnect policy
	// for you automatically.
	cm := xmpp.NewStreamManager(xmppClient, func(c xmpp.Sender) {
		joinMUC(c)
	})
	
	go func() {
		for  {
			msg := <- client.sendChannel
			sendMessage(client.xmppClient, msg.XMPPMessage)
		}
	}()
	
	go func() {
		log.Fatal(cm.Run())
	}()
	clientID := genID()
	clients[clientID] = client
	return clientID
}

func sendMessage(xmppClient *xmpp.Client, message *XMPPMessage)  {
	msg := stanza.NewMessage(stanza.Attrs{
		Id:   genID(),
		From: xmppClient.Session.BindJid,
		To:   message.To,
		Type: stanza.MessageTypeGroupchat,
	})
	msg.Body = message.Body

	err := xmppClient.Send(msg)
	if err != nil {
		log.Fatal("Error on sending XMPPMessage: " + err.Error())
	}
}


func joinMUC(s xmpp.Sender) {
	client, ok := s.(*xmpp.Client)
	if !ok {
		log.Fatal("post connect sender not a client, cannot proceed")
	}
	id := genID()
	//prepare presence for joining the MUC
	presence := stanza.NewPresence(stanza.Attrs{
		To:   "super-test-channel@conference.fem-net.de/bot-"+id,//fill the fields accordingly
		From: client.Session.BindJid,
		Id:   id,
	})

	//as stated in the XEP0045 documentation, you have to tell that you are able to speak muc
	presence.Extensions = append(presence.Extensions, stanza.MucPresence{})

	//send the stuff and actually join the MUC
	err := client.Send(presence)
	if err != nil {
		log.Fatal(err)
	}
}

func errorHandler(err error) {
	log.Fatal(err.Error())
}