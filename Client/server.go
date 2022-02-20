package main

import (
	logger "Distributed_Web_Crawler/Logger"
	mq "Distributed_Web_Crawler/MessageQueue"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)


var upgrader = websocket.Upgrader{
	ReadBufferSize: 1024, //requests are usually small
	WriteBufferSize: 1024 * 16, //response is usually pretty large
}

var	connsMap map[string]*Client  //keep track of all clients and their connections, to be able to send on them
var	mu sync.RWMutex
var q *mq.MQ   								//message queue to publish and consume messages


func main(){
	q = mq.New("amqp://guest:guest@" + mqHost + ":" + mqPort + "/")  //os.Getenv("AMQP_URL"))

	mux := http.NewServeMux()
	mux.HandleFunc("/job", serveWS)

	logger.LogInfo(logger.SERVER, "Listening on %v:%v", myHost, mqPort)
	
	err := http.ListenAndServe(myHost + ":" + myPort, &LoggingMiddleware{mux})
	if err != nil{
		logger.FailOnError(logger.SERVER, "Failed in listening on port with error %v", err)
	}
}

func serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.LogError(logger.SERVER, "Unable to upgrade http request to use websockets with error %v", err)
		return
	}
	
	client, err := NewClient(conn)
	if err != nil{
		logger.FailOnError(logger.SERVER, "Unable to create client with error %v", err)
		return
	}

	mu.Lock()
	connsMap[client.id] = client
	mu.Unlock()

	go client.reader()
	go qConsumer()
}

//read client job requests, and dump them to rabbit mq
func (c *Client) reader(){
	defer c.conn.Close()

	for{
		select{
		case <- c.killChan:
			//die
			return
		default:
			c.conn.SetReadDeadline(time.Now().Add(MAX_IDLE_TIME))  //can be idle for at most 10 mins
			_, message, err := c.conn.ReadMessage()
			if err != nil{
				logger.LogError(logger.SERVER, "Error %v with client %v", err, c.conn.RemoteAddr())
				return
			}

			//read message
			newJob := &mq.Job{}
			err = json.Unmarshal(message, newJob)
			if err != nil{
				logger.LogError(logger.SERVER, "Error %v with client %v", err, c.conn.RemoteAddr())
				return
			}

			//TODO
			//make sure job not present in cache
			

			//message is viable, can now send it over to mq
			err = q.Publish(mq.JOBS_QUEUE, message)
			if err != nil{
				logger.LogError(logger.SERVER, "New job not published to queue with err %v", err)
			}else{
				logger.LogInfo(logger.SERVER, "New job successfully published to queue")
			}
		}
	}
}

//write given data to a given connection
func writer(conn *websocket.Conn, data interface{}){
	conn.WriteJSON(data)
}

//remove all connections idle for more than 1 hour
func cleaner(){

}

//
// start a thread that waits on a doneJobs from the message queue
//
func qConsumer() {
	ch, err := q.Consume(mq.DONE_JOBS_QUEUE)

	if err != nil{
		logger.FailOnError(logger.SERVER, "Server can't consume doneJobs because with this error %v", err)
	}

	for {		
		select{
		case doneJob := <- ch:  //job has been finished and pushed to queue

			body := doneJob.Body
			data := &mq.DoneJob{}
			
			err := json.Unmarshal(body, data)
			if err != nil {
				logger.LogError(logger.MASTER, "Unable to unMarshal job with error %v\nWill discard it", err) 
				doneJob.Ack(false)
				continue
			}

			//a job has been finished, now need to push it over
			//appropriate connection

			mu.RLock()
			client, ok := connsMap[data.ClientId]
			mu.RUnlock()


			if ok{
				//send results to client over conn
				go writer(client.conn, data)
			} 
			//else, connection with client has already been terminated

			doneJob.Ack(false)

			//TODO add  job to cache
			
		default:
			logger.LogInfo(logger.SERVER, "No jobs found, about to sleep") 
			time.Sleep(time.Second * 5)
		}	
	}
}